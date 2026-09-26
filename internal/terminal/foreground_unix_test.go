//go:build darwin || linux

// These tests pin ForegroundPTY against real child processes: the test
// binary re-executed in one of the helper modes below, on a real PTY, with
// another PTY pair standing in for the user's terminal.
package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"golang.org/x/term"
)

const (
	helperModeEnv = "AGENTSCTL_FOREGROUND_HELPER"
	helperDirEnv  = "AGENTSCTL_FOREGROUND_HELPER_DIR"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		runForegroundHelper(mode, os.Getenv(helperDirEnv))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runForegroundHelper is the child side. Every mode that waits for input
// or a signal first prints "ready" and writes its PID to dir/pid.
//
//   - record: appends everything it reads to dir/input; on SIGHUP it lets
//     pending input land, then exits 129.
//   - group: starts a stubborn grandchild in its own process group, then
//     blocks with the default SIGHUP action.
//   - ignore-hup: ignores SIGHUP.
//   - stubborn: ignores SIGHUP and SIGTERM.
//   - size: prints its terminal size now and on every SIGWINCH.
//   - exit: prints "bye" and exits 3.
//   - editor: prints a Codex-to-external-editor round trip and exits 0.
func runForegroundHelper(mode, dir string) {
	writePID := func(name string) {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	ready := func() {
		writePID("pid")
		fmt.Print("ready")
	}
	switch mode {
	case "record":
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		f, _ := os.OpenFile(filepath.Join(dir, "input"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				_, _ = f.Write(buf[:n])
				if err != nil {
					return
				}
			}
		}()
		ready()
		<-hup
		time.Sleep(100 * time.Millisecond)
		os.Exit(129)
	case "group":
		grandchild := exec.Command(os.Args[0])
		grandchild.Env = append(os.Environ(), helperModeEnv+"=grandchild")
		if err := grandchild.Start(); err != nil {
			os.Exit(1)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "grandchild")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		ready()
		select {}
	case "grandchild":
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
		writePID("grandchild")
		select {}
	case "ignore-hup":
		signal.Ignore(syscall.SIGHUP)
		ready()
		select {}
	case "stubborn":
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
		ready()
		select {}
	case "size":
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		ready()
		for {
			rows, cols, _ := creackpty.Getsize(os.Stdin)
			fmt.Printf("[size %d %d]", rows, cols)
			<-winch
		}
	case "exit":
		fmt.Print("bye")
		os.Exit(3)
	case "editor":
		for _, out := range []string{"Codex output", BracketedPasteDisable, AlternateScreenEnable, "editor output", AlternateScreenDisable, BracketedPasteEnable, "Codex redraw/output"} {
			fmt.Print(out)
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// foregroundRun is one ForegroundPTY.Run in progress against a fake user
// terminal.
type foregroundRun struct {
	t      *testing.T
	dir    string
	user   *os.File // what the user types into
	screen *screenModel
	done   chan error
}

func startForeground(t *testing.T, ctx context.Context, mode string, screen *screenModel) *foregroundRun {
	t.Helper()
	master, slave := newTerminalPair(t)
	dir := t.TempDir()
	t.Setenv(helperModeEnv, mode)
	t.Setenv(helperDirEnv, dir)
	r := &foregroundRun{t: t, dir: dir, user: master, screen: screen, done: make(chan error, 1)}
	go func() {
		r.done <- ForegroundPTY{StopTimeout: 100 * time.Millisecond}.Run(ctx, os.Args[0], nil, dir, slave, screen)
	}()
	return r
}

// startReady starts mode and waits for it to be up and relayed.
func startReady(t *testing.T, mode string, screen *screenModel) *foregroundRun {
	t.Helper()
	r := startForeground(t, context.Background(), mode, screen)
	r.waitScreen("ready")
	return r
}

func (r *foregroundRun) waitScreen(text string) {
	r.t.Helper()
	waitFor(r.t, "screen to show "+strconv.Quote(text), func() bool {
		_, alternate, _ := r.screen.state()
		return strings.Contains(alternate, text)
	})
}

func (r *foregroundRun) typeBytes(b []byte) {
	r.t.Helper()
	if _, err := r.user.Write(b); err != nil {
		r.t.Fatal(err)
	}
}

func (r *foregroundRun) result() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		r.t.Fatal("Run did not return")
		return nil
	}
}

func (r *foregroundRun) pid(name string) int {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		r.t.Fatal(err)
	}
	return pid
}

func (r *foregroundRun) input() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "input"))
	return string(b)
}

// gone reports whether pid no longer exists. A zombie still exists, so an
// unreaped direct child is not gone.
func gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// assertReleased checks the user's terminal is as it was before Run.
func assertReleased(t *testing.T, screen *screenModel, shell string) {
	t.Helper()
	main, _, alternate := screen.state()
	if alternate || screen.pasteOn() {
		t.Fatalf("terminal left with alternate screen=%v bracketed paste=%v", alternate, screen.pasteOn())
	}
	if main != shell {
		t.Fatalf("main screen = %q, want only %q", main, shell)
	}
}

func TestForegroundPTYLiteralDetachDeliversOnlyBytesBeforeIt(t *testing.T) {
	screen := &screenModel{}
	r := startReady(t, "record", screen)
	r.typeBytes([]byte("abc\x1dxyz"))
	if err := r.result(); err != nil {
		t.Fatalf("Run = %v, want nil for a detach", err)
	}
	if got := r.input(); got != "abc" {
		t.Fatalf("child input = %q, want %q", got, "abc")
	}
	if !gone(r.pid("pid")) {
		t.Fatal("child not reaped after detach")
	}
	assertReleased(t, screen, "")
}

func TestForegroundPTYDetachesOnModifyOtherKeysEncoding(t *testing.T) {
	r := startReady(t, "record", &screenModel{})
	r.typeBytes([]byte("a\x1b[27;5;93~b"))
	if err := r.result(); err != nil {
		t.Fatalf("Run = %v, want nil for a detach", err)
	}
	if got := r.input(); got != "a" {
		t.Fatalf("child input = %q, want %q", got, "a")
	}
}

// The transport enables bracketed paste, so the terminal marks a paste; a
// pasted Ctrl+] is content, and only the physical key afterwards detaches.
func TestForegroundPTYPastedDetachKeyIsDeliveredNotDetached(t *testing.T) {
	screen := &screenModel{}
	r := startReady(t, "record", screen)
	if !screen.pasteOn() {
		t.Fatal("bracketed paste not enabled while the child runs")
	}
	paste := "\x1b[200~x\x1dy\x1b[27;5;93~z\x1b[201~"
	r.typeBytes([]byte(paste))
	waitFor(t, "paste to reach the child", func() bool { return r.input() == paste })
	select {
	case err := <-r.done:
		t.Fatalf("Run returned %v on a pasted detach key", err)
	default:
	}
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
	if got := r.input(); got != paste {
		t.Fatalf("child input = %q, want the paste only", got)
	}
}

func TestForegroundPTYDeliversInputByteForByte(t *testing.T) {
	r := startReady(t, "record", &screenModel{})
	var want []byte
	for b := 0; b < 256; b++ {
		if byte(b) != DetachKey {
			want = append(want, byte(b))
		}
	}
	r.typeBytes(want)
	waitFor(t, "all input to reach the child", func() bool { return len(r.input()) >= len(want) })
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
	if got := r.input(); got != string(want) {
		t.Fatalf("child input = %q, want %q", got, want)
	}
}

// A signal-induced exit caused by the detach is not a failure, and the
// whole process group goes, including a member that ignores SIGHUP and
// SIGTERM.
func TestForegroundPTYDetachEndsTheWholeProcessGroup(t *testing.T) {
	r := startReady(t, "group", &screenModel{})
	grandchild := r.pid("grandchild")
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatalf("Run = %v, want nil for a detach", err)
	}
	if !gone(r.pid("pid")) {
		t.Fatal("child not reaped after detach")
	}
	waitFor(t, "grandchild to be gone", func() bool { return gone(grandchild) })
}

func TestForegroundPTYDetachEscalatesPastIgnoredSignals(t *testing.T) {
	for _, mode := range []string{"ignore-hup", "stubborn"} {
		t.Run(mode, func(t *testing.T) {
			screen := &screenModel{}
			r := startReady(t, mode, screen)
			r.typeBytes([]byte{DetachKey})
			if err := r.result(); err != nil {
				t.Fatalf("Run = %v, want nil for a detach", err)
			}
			if !gone(r.pid("pid")) {
				t.Fatal("child not reaped after detach")
			}
			assertReleased(t, screen, "")
		})
	}
}

func TestForegroundPTYChildExitIsTheResult(t *testing.T) {
	screen := &screenModel{}
	r := startForeground(t, context.Background(), "exit", screen)
	err := r.result()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("Run = %v, want exit status 3", err)
	}
	_, alternate, _ := screen.state()
	if !strings.Contains(alternate, "bye") {
		t.Fatalf("alternate screen = %q, want the child's last output", alternate)
	}
	assertReleased(t, screen, "")
}

func TestForegroundPTYStartFailureLeavesTerminalUntouched(t *testing.T) {
	_, slave := newTerminalPair(t)
	screen := &screenModel{}
	err := ForegroundPTY{}.Run(context.Background(), filepath.Join(t.TempDir(), "missing"), nil, "", slave, screen)
	if err == nil {
		t.Fatal("Run succeeded without a child")
	}
	if screen.written() != "" {
		t.Fatalf("terminal output = %q, want none", screen.written())
	}
}

func TestForegroundPTYContextCancelReapsAndReportsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	screen := &screenModel{}
	r := startForeground(t, ctx, "stubborn", screen)
	r.waitScreen("ready")
	cancel()
	if err := r.result(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if !gone(r.pid("pid")) {
		t.Fatal("child not reaped after cancel")
	}
	assertReleased(t, screen, "")
}

func TestForegroundPTYOutputFailureStillReleasesTerminal(t *testing.T) {
	screen := &screenModel{failContaining: "ready"}
	r := startForeground(t, context.Background(), "stubborn", screen)
	if err := r.result(); err == nil || !strings.Contains(err.Error(), "terminal write failed") {
		t.Fatalf("Run = %v, want the output failure", err)
	}
	if !gone(r.pid("pid")) {
		t.Fatal("child not reaped after output failure")
	}
	assertReleased(t, screen, "")
}

// Paste and screen are released only after the child is reaped, which is
// also after the last child output was forwarded, and the terminal mode is
// restored only after both.
func TestForegroundPTYReleasesModesOnlyAfterChildIsGone(t *testing.T) {
	var pid atomic.Int64
	var early atomic.Bool
	var modeRestoredEarly atomic.Bool
	master, slave := newTerminalPair(t)
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	screen := &screenModel{beforeWrite: func(b []byte) {
		if s := string(b); s == BracketedPasteDisable || s == AlternateScreenDisable {
			if p := pid.Load(); p == 0 || !gone(int(p)) {
				early.Store(true)
			}
			current, err := term.GetState(int(slave.Fd()))
			if err != nil || reflect.DeepEqual(current, original) {
				modeRestoredEarly.Store(true)
			}
		}
	}}
	dir := t.TempDir()
	t.Setenv(helperModeEnv, "record")
	t.Setenv(helperDirEnv, dir)
	done := make(chan error, 1)
	go func() {
		done <- ForegroundPTY{StopTimeout: 100 * time.Millisecond}.Run(context.Background(), os.Args[0], nil, dir, slave, screen)
	}()
	r := &foregroundRun{t: t, dir: dir, user: master, screen: screen, done: done}
	r.waitScreen("ready")
	pid.Store(int64(r.pid("pid")))
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
	if early.Load() {
		t.Fatal("terminal modes released while the child was still running")
	}
	if modeRestoredEarly.Load() {
		t.Fatal("terminal mode restored before the screen was released")
	}
	restored, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("terminal mode not restored")
	}
}

func TestForegroundPTYNeverDepositsAFrameOnTheMainScreen(t *testing.T) {
	screen := &screenModel{}
	_, _ = screen.Write([]byte("shell"))
	r := startReady(t, "record", screen)
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
	// Agent View takes its own alternate screen back, then releases it for
	// the next foreground provider: the shell must be all that shows.
	_, _ = screen.Write([]byte(AlternateScreenEnable + "Agent View" + AlternateScreenDisable))
	assertReleased(t, screen, "shell")
}

// The child's own screen leave (here an external editor's) must not expose
// the main screen; everything else it emits passes through in order.
func TestForegroundPTYKeepsScreenOwnershipAcrossExternalEditorLifecycle(t *testing.T) {
	screen := &screenModel{}
	_, _ = screen.Write([]byte("shell"))
	r := startForeground(t, context.Background(), "editor", screen)
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
	want := "shell" + AlternateScreenEnable + BracketedPasteEnable + "Codex output" + BracketedPasteDisable + AlternateScreenEnable + "editor output" + BracketedPasteEnable + "Codex redraw/output" + BracketedPasteDisable + AlternateScreenDisable
	if got := screen.written(); got != want {
		t.Fatalf("terminal output = %q, want %q", got, want)
	}
	assertReleased(t, screen, "shell")
}

func TestForegroundPTYFollowsTerminalSize(t *testing.T) {
	master, slave := newTerminalPair(t)
	if err := creackpty.Setsize(master, &creackpty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(helperModeEnv, "size")
	t.Setenv(helperDirEnv, dir)
	screen := &screenModel{}
	done := make(chan error, 1)
	go func() {
		done <- ForegroundPTY{StopTimeout: 100 * time.Millisecond}.Run(context.Background(), os.Args[0], nil, dir, slave, screen)
	}()
	r := &foregroundRun{t: t, dir: dir, user: master, screen: screen, done: done}
	r.waitScreen("[size 30 100]")

	if err := creackpty.Setsize(master, &creackpty.Winsize{Rows: 40, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	// The user's terminal is not this process's controlling terminal, so
	// deliver the SIGWINCH its resize would have raised.
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	r.waitScreen("[size 40 120]")
	r.typeBytes([]byte{DetachKey})
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
}

// screenModel is the user's terminal reduced to what these tests check:
// bracketed paste and which of the main and alternate screens text lands
// on. It recognizes only the mode sequences ForegroundPTY owns.
type screenModel struct {
	mu             sync.Mutex
	output         bytes.Buffer
	main           bytes.Buffer
	alternate      bytes.Buffer
	pending        []byte
	alternateOn    bool
	paste          bool
	failContaining string
	beforeWrite    func([]byte)
}

func (m *screenModel) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failContaining != "" && bytes.Contains(b, []byte(m.failContaining)) {
		return 0, errors.New("terminal write failed")
	}
	if m.beforeWrite != nil {
		m.beforeWrite(b)
	}
	m.output.Write(b)
	on := bytes.LastIndex(b, []byte(BracketedPasteEnable))
	off := bytes.LastIndex(b, []byte(BracketedPasteDisable))
	if on >= 0 || off >= 0 {
		m.paste = on > off
	}
	data := append(m.pending, b...)
	m.pending = nil
	for len(data) > 0 {
		enter := bytes.Index(data, []byte(AlternateScreenEnable))
		leave := bytes.Index(data, []byte(AlternateScreenDisable))
		next, entering := enter, true
		if leave >= 0 && (enter < 0 || leave < enter) {
			next, entering = leave, false
		}
		if next < 0 {
			keep := 0
			for n := 1; n < len(AlternateScreenEnable) && n <= len(data); n++ {
				if tail := string(data[len(data)-n:]); tail == AlternateScreenEnable[:n] || tail == AlternateScreenDisable[:n] {
					keep = n
				}
			}
			m.screenText(data[:len(data)-keep])
			m.pending = append(m.pending, data[len(data)-keep:]...)
			break
		}
		m.screenText(data[:next])
		if entering {
			m.alternate.Reset()
		}
		m.alternateOn = entering
		data = data[next+len(AlternateScreenEnable):]
	}
	return len(b), nil
}

func (m *screenModel) screenText(b []byte) {
	if m.alternateOn {
		m.alternate.Write(b)
		return
	}
	m.main.Write(b)
}

func (m *screenModel) state() (main, alternate string, alternateOn bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.main.String(), m.alternate.String(), m.alternateOn
}

func (m *screenModel) pasteOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paste
}

func (m *screenModel) written() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.output.String()
}

func newTerminalPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	return master, slave
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
