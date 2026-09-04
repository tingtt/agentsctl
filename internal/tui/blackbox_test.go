//go:build darwin || linux

package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"golang.org/x/sys/unix"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestBuiltBinaryRealPTYJourney(t *testing.T) {
	moduleRoot := moduleRoot(t)
	// A short prefix keeps the fixture's depth-2 CWD ("<root-basename>/project")
	// well inside the title budget the new right-block-first row layout
	// leaves at the narrower 48-column size exercised below (that layout
	// intentionally shows the CWD in full before giving the title any
	// remaining width, so an unnecessarily long temp-dir name would
	// squeeze the title far more than a realistic project path ever would).
	root, err := os.MkdirTemp("/tmp", "e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	probe, err := net.Listen("unix", filepath.Join(root, "probe.sock"))
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	_ = probe.Close()
	_ = os.Remove(filepath.Join(root, "probe.sock"))
	bin := filepath.Join(root, "bin")
	project := filepath.Join(root, "project")
	fakeDir := filepath.Join(root, "fake")
	stateDir := filepath.Join(root, "state")
	for _, dir := range []string{bin, project, fakeDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "agentsctl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/agentsctl")
	build.Dir = moduleRoot
	build.Env = environmentForTest(os.Environ(), "GOCACHE", filepath.Join(root, "go-cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentsctl: %v\n%s", err, output)
	}
	for _, name := range []string{"codex", "claude"} {
		if err := os.Symlink(filepath.Join(moduleRoot, "internal", "testkit", "fakecli", name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCatalogFixture(fakeDir, project, 100); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "run-agentsctl")
	wrapperSource := "#!/bin/sh\nstty -g > \"$AGENTSCTL_FAKE_DIR/before.stty\"\n\"$AGENTSCTL_TEST_BINARY\"\nresult=$?\nstty -g > \"$AGENTSCTL_FAKE_DIR/after.stty\"\nexit $result\n"
	if err := os.WriteFile(wrapper, []byte(wrapperSource), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper)
	command.Dir = project
	command.Env = append(os.Environ(),
		"AGENTSCTL_FAKE_DIR="+fakeDir,
		"AGENTSCTL_STATE_DIR="+stateDir,
		"AGENTSCTL_TEST_BINARY="+binary,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	t.Cleanup(func() { stopTestDaemon(stateDir) })
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	var output lockedBuffer
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, terminal); close(readDone) }()

	if err := waitOutput(&output, "session-099", 8*time.Second); err != nil {
		if strings.Contains(strings.ToLower(output.String()), "operation not permitted") {
			t.Skipf("sandbox does not permit daemon/PTY boundary: %s", output.String())
		}
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 80, 24, "session-099", "Ctrl+X", "claude >")

	// Composer left/right/home/end/insert/backspace/delete, including
	// crossing a full-width Japanese rune, driven by real terminal escape
	// sequences written to a real PTY (not the synthetic
	// bufio.NewReader(strings.NewReader(...)) unit tests, which never
	// exercise the kernel pty timing this goes through).
	writePTY(t, terminal, "abc")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"abc"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("composer did not accept typed text: %v", err)
	}
	writePTY(t, terminal, "\x1b[D\x1b[D") // left, left
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"a"+cursorStyle("b")+"c")
	}); err != nil {
		t.Fatalf("left arrow did not move the composer cursor: %v", err)
	}
	writePTY(t, terminal, "X")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"aX"+cursorStyle("b")+"c")
	}); err != nil {
		t.Fatalf("mid-composer insert failed: %v", err)
	}
	writePTY(t, terminal, "\x1b[C") // right
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"aXb"+cursorStyle("c"))
	}); err != nil {
		t.Fatalf("right arrow did not move the composer cursor: %v", err)
	}
	writePTY(t, terminal, "\x1b[H") // home
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle("a")+"Xbc")
	}); err != nil {
		t.Fatalf("home did not move the composer cursor to the start: %v", err)
	}
	writePTY(t, terminal, "\x1b[F") // end
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"aXbc"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("end did not move the composer cursor to the end: %v", err)
	}
	writePTY(t, terminal, "\x7f") // backspace
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"aXb"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("backspace did not delete a composer character: %v", err)
	}
	writePTY(t, terminal, "\x1b[D\x1b[D\x1b[D") // left x3, reaching the start
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle("a")+"Xb")
	}); err != nil {
		t.Fatalf("left arrow did not reach the composer start: %v", err)
	}
	writePTY(t, terminal, "\x1b[3~") // delete
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle("X")+"b")
	}); err != nil {
		t.Fatalf("delete did not remove a composer character: %v", err)
	}
	writePTY(t, terminal, "界") // insert a full-width rune at the start
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"界"+cursorStyle("X")+"b")
	}); err != nil {
		t.Fatalf("Japanese character insert into the composer failed: %v", err)
	}
	writePTY(t, terminal, "\x1b[C") // right, crossing past the inserted rune
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"界X"+cursorStyle("b"))
	}); err != nil {
		t.Fatalf("right arrow did not cross the Japanese character: %v", err)
	}
	writePTY(t, terminal, "\x1b[D\x1b[D") // left, left, landing back on the Japanese character
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle("界")+"Xb")
	}); err != nil {
		t.Fatalf("left arrow did not cross back onto the Japanese character: %v", err)
	}
	writePTY(t, terminal, "\x1b[F"+strings.Repeat("\x7f", 3)) // end, then clear the composer for later steps
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("composer was not cleared after the arrow-key exercise: %v", err)
	}

	// SS3-form arrow keys (ESC O <letter>): terminfo for TERM=xterm-256color
	// (a common default, including on macOS terminals) declares
	// kcub1/kcuf1 (Left/Right) as \EOD/\EOC rather than the CSI form
	// \E[D/\E[C exercised above. A terminal sending this form must move
	// the composer cursor too.
	writePTY(t, terminal, "hi")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"hi"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("composer did not accept typed text: %v", err)
	}
	writePTY(t, terminal, "\x1bOD") // SS3 left
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"h"+cursorStyle("i"))
	}); err != nil {
		t.Fatalf("SS3-form left arrow did not move the composer cursor: %v", err)
	}
	writePTY(t, terminal, "!")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"h!"+cursorStyle("i"))
	}); err != nil {
		t.Fatalf("mid-composer insert after SS3 left arrow failed: %v", err)
	}
	writePTY(t, terminal, "\x1bOC") // SS3 right
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+"h!i"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("SS3-form right arrow did not move the composer cursor: %v", err)
	}
	writePTY(t, terminal, strings.Repeat("\x7f", 3)) // clear the composer again
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderClaude, "")+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("composer was not cleared after the SS3 arrow-key exercise: %v", err)
	}

	// Ctrl+A is no longer bound to anything (superseded by the Ctrl+G
	// 3-state scope cycle below): it must not change the scope or leak the
	// sibling-directory session into view.
	preCtrlA := latestFrame(output.String())
	writePTY(t, terminal, "\x01")
	if err := waitLatestFrame(&output, time.Second, func(frame string) bool { return frame != preCtrlA }); err == nil {
		t.Fatalf("Ctrl+A (no longer bound) changed the frame:\nbefore:\n%s\nafter:\n%s", preCtrlA, latestFrame(output.String()))
	}
	if strings.Contains(latestFrame(output.String()), "MUST-NOT-SHOW-SIBLING") {
		t.Fatal("Ctrl+A exposed the sibling-directory session")
	}

	// Ctrl+G: cwd -> cwd/** (the sibling directory is not a descendant of
	// cwd, so it must still not appear).
	writePTY(t, terminal, "\x07")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "agentsctl · cwd/**")
	}); err != nil {
		t.Fatalf("Ctrl+G did not switch to the cwd/** scope:\n%s", latestFrame(output.String()))
	}
	if strings.Contains(latestFrame(output.String()), "MUST-NOT-SHOW-SIBLING") {
		t.Fatal("cwd/** scope exposed a sibling directory outside the subtree")
	}
	if strings.Contains(output.String(), "FAKE CODEX ATTACHED") {
		t.Fatal("Ctrl+G attached a session")
	}

	// Ctrl+G: cwd/** -> all (no directory filter; the sibling now appears).
	writePTY(t, terminal, "\x07")
	if err := waitOutput(&output, "MUST-NOT-SHOW-SIBLING", 3*time.Second); err != nil {
		t.Fatal("Ctrl+G did not expose all directories")
	}
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "agentsctl · all")
	}); err != nil {
		t.Fatalf("Ctrl+G did not switch to the all scope:\n%s", latestFrame(output.String()))
	}

	// Ctrl+G: all -> cwd (back to the start; the sibling disappears again).
	writePTY(t, terminal, "\x07")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "agentsctl · cwd") && !strings.Contains(frame, "cwd/**") && !strings.Contains(frame, "MUST-NOT-SHOW-SIBLING")
	}); err != nil {
		t.Fatalf("Ctrl+G did not wrap back to the cwd scope:\n%s", latestFrame(output.String()))
	}

	writePTY(t, terminal, "\x1b[Z")
	if err := waitOutput(&output, composerPrefix(session.ProviderCodex, ""), 3*time.Second); err != nil {
		t.Fatal(err)
	}

	writePTY(t, terminal, "First\x13Second\x13")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"First")
	}); err != nil {
		t.Fatalf("shared stash did not restore First for Codex: %v", err)
	}
	writePTY(t, terminal, "\x13")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"Second")
	}); err != nil {
		t.Fatalf("second stash swap did not restore Second: %v", err)
	}
	writePTY(t, terminal, strings.Repeat("\x7f", len("Second")))
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		// An empty composer places the cursor glyph directly after the
		// prompt prefix with nothing in between.
		return strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("composer did not become empty: %v", err)
	}

	frameCount := strings.Count(output.String(), "\x1b[2J\x1b[H")
	writePTY(t, terminal, "\x16\x1b[55;5u")
	if err := waitLatestFrame(&output, 3*time.Second, func(string) bool {
		return strings.Count(output.String(), "\x1b[2J\x1b[H") >= frameCount+2
	}); err != nil {
		t.Fatal(err)
	}
	screen := latestFrame(output.String())
	if strings.Contains(screen, "archived") || strings.Contains(screen, "55;5u") || strings.Contains(screen, "7;5u") {
		t.Fatalf("unsupported input leaked into screen:\n%s", screen)
	}

	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 10, Cols: 48}); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return len(frameLines(frame)) == 10
	}); err != nil {
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 48, 10, "session-099", "Ctrl+X", "codex >")
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")

	writePTY(t, terminal, "\r")
	if err := waitOutput(&output, "FAKE CODEX ATTACHED", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	run := waitRunningRun(t, filepath.Join(stateDir, "state.json"))
	overviewCount := strings.Count(output.String(), "agentsctl · cwd")
	writePTY(t, terminal, "\x13\x1d")
	if err := waitOutputAfter(&output, "agentsctl · cwd", overviewCount+1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// A successful attach return -- via AttachCodex returning with no
	// error and App.act's Model.MarkAttached call, regardless of how the
	// attachment ended -- marks session-099 as last-attached: still
	// selected, it must render bold on the very next overview frame. The
	// fake CLI leaves the row's status "active" (ActivityWorking) across a
	// detach -- it only resets to "completed" when the underlying process
	// actually exits.
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, rowPrefix(">", session.ActivityWorking, true, true)+"session-099")
	}); err != nil {
		t.Fatalf("returning to overview after attach did not mark the session bold: %v", err)
	}
	childInput, err := os.ReadFile(filepath.Join(fakeDir, "child-input.bin"))
	if err != nil || !bytes.Contains(childInput, []byte{0x13}) {
		t.Fatalf("Ctrl+S was not forwarded to child: input=%v err=%v", childInput, err)
	}
	if err := syscall.Kill(run.PID, 0); err != nil {
		t.Fatalf("child did not survive detach: %v", err)
	}
	redrawCount := strings.Count(output.String(), "FAKE CODEX REDRAW")
	writePTY(t, terminal, "\r")
	if err := waitOutputAfter(&output, "FAKE CODEX REDRAW", redrawCount+1, 5*time.Second); err != nil {
		t.Fatalf("Codex reattach did not request a full redraw: %v", err)
	}
	writePTY(t, terminal, "\x1d")
	if err := waitOutputAfter(&output, "agentsctl · cwd", overviewCount+2, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	writePTY(t, terminal, "Prompt\x12\x1b[H\x1b[3~X\x1b")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "session-099") && strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"Prompt") && strings.Contains(frame, "Rename cancelled")
	}); err != nil {
		t.Fatalf("rename cancel did not preserve row and composer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fakeDir, "rename.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel invoked provider rename: %v", err)
	}
	writePTY(t, terminal, "\x12\x1b[H\x1b[3~X\r")
	if err := waitLatestFrame(&output, 5*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-099") && strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"Prompt")
	}); err != nil {
		t.Fatalf("rename success did not refresh the same row: %v", err)
	}
	renameLog, err := os.ReadFile(filepath.Join(fakeDir, "rename.log"))
	if err != nil || strings.Count(strings.TrimSpace(string(renameLog)), "\n") != 0 || !strings.Contains(string(renameLog), `"id": "session-099"`) || !strings.Contains(string(renameLog), `"name": "Xession-099"`) {
		t.Fatalf("provider rename calls=%q err=%v", renameLog, err)
	}

	// Rename editor left/right/home/end, mid-word insert, and crossing a
	// full-width Japanese rune, again over a real PTY.
	writePTY(t, terminal, "\x12")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-099"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("rename editor did not open with the cursor at the end: %v", err)
	}
	writePTY(t, terminal, "\x1b[D\x1b[D\x1b[D") // left x3
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-"+styledCursorSuffix(true, true, "0", "99"))
	}); err != nil {
		t.Fatalf("left arrow did not move the rename cursor: %v", err)
	}
	writePTY(t, terminal, "Z")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-Z"+styledCursorSuffix(true, true, "0", "99"))
	}); err != nil {
		t.Fatalf("mid-rename insert failed: %v", err)
	}
	writePTY(t, terminal, "\x1b[C") // right
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-Z0"+styledCursorSuffix(true, true, "9", "9"))
	}); err != nil {
		t.Fatalf("right arrow did not move the rename cursor: %v", err)
	}
	writePTY(t, terminal, "\x1b[H") // home
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, styledCursorSuffix(true, true, "X", "ession-Z099"))
	}); err != nil {
		t.Fatalf("home did not move the rename cursor to the start: %v", err)
	}
	writePTY(t, terminal, "\x1b[F") // end
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-Z099"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("end did not move the rename cursor to the end: %v", err)
	}
	writePTY(t, terminal, "界") // insert a full-width rune at the end
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-Z099界"+cursorStyle(" "))
	}); err != nil {
		t.Fatalf("Japanese character insert into the rename editor failed: %v", err)
	}
	writePTY(t, terminal, "\x1b[D") // left, landing on the Japanese character
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-Z099"+cursorStyle("界"))
	}); err != nil {
		t.Fatalf("left arrow did not cross onto the Japanese character in rename: %v", err)
	}
	writePTY(t, terminal, "\x1b") // cancel; must not call provider.Rename again
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "Xession-099") && strings.Contains(frame, "Rename cancelled")
	}); err != nil {
		t.Fatalf("second rename cancel did not restore the committed name: %v", err)
	}
	if renameLogAfterCursorTest, err := os.ReadFile(filepath.Join(fakeDir, "rename.log")); err != nil || string(renameLogAfterCursorTest) != string(renameLog) {
		t.Fatalf("cancelled rename invoked provider rename again: %q vs %q (err=%v)", renameLogAfterCursorTest, renameLog, err)
	}

	writePTY(t, terminal, "\x13")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool { return strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"First") }); err != nil {
		t.Fatalf("rename changed stash: %v", err)
	}
	writePTY(t, terminal, "\x13")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool { return strings.Contains(frame, composerPrefix(session.ProviderCodex, "")+"Prompt") }); err != nil {
		t.Fatalf("rename changed composer: %v", err)
	}
	persistedState, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"First", "Second", "Prompt"} {
		if bytes.Contains(persistedState, []byte(text)) {
			t.Fatalf("prompt or stash text %q was persisted in state: %s", text, persistedState)
		}
	}

	writePTY(t, terminal, "\x18")
	waitRunStopped(t, filepath.Join(stateDir, "state.json"), run.ID)
	if err := syscall.Kill(run.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child remained alive after Stop: %v", err)
	}
	// The archive confirmation must appear on the target session's own row
	// (immediately before the provider/cwd block), in red — not as a
	// footer/composer-area status line.
	confirmText := styleText("Press Ctrl+X again to archive", colorRed)
	writePTY(t, terminal, "\x18")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		for _, line := range strings.Split(frame, "\r\n") {
			if strings.Contains(line, "Xession-099") && strings.Contains(line, confirmText) {
				return true
			}
		}
		return false
	}); err != nil {
		t.Fatalf("archive confirmation was not shown, in red, on the target session row:\n%s", latestFrame(output.String()))
	}
	if strings.Contains(latestFrame(output.String()), composerPrefix(session.ProviderCodex, "")+confirmText) {
		t.Fatal("archive confirmation leaked into the composer/footer area")
	}
	writePTY(t, terminal, "\x18")
	waitArchived(t, filepath.Join(fakeDir, "codex.json"), "session-099", &output)
	if err := os.WriteFile(filepath.Join(fakeDir, "codex.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeFixture, _ := json.Marshal([]map[string]any{{
		"id": "claude-live", "name": "fake-claude-live", "summary": "Working", "cwd": project,
		"updatedAt": time.Now().Unix(), "status": "busy", "state": "working",
	}})
	if err := os.WriteFile(filepath.Join(fakeDir, "claude.json"), claudeFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitOutput(&output, "fake-claude-live", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, strings.Repeat("\x7f", len("Prompt")))
	writePTY(t, terminal, "\r")
	if err := waitOutput(&output, "FAKE CLAUDE ATTACHED", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	overviewCount = strings.Count(output.String(), "agentsctl · cwd")
	writePTY(t, terminal, "\x1d")
	if err := waitOutputAfter(&output, "agentsctl · cwd", overviewCount+1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	claudeData, err := os.ReadFile(filepath.Join(fakeDir, "claude.json"))
	if err != nil || !strings.Contains(string(claudeData), "working") {
		t.Fatalf("Claude session did not survive detach: data=%s err=%v", claudeData, err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "codex.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitOutput(&output, "unavailable:", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 80, 24, "fake-claude-live", "unavailable:", "Ctrl+X")

	writePTY(t, terminal, "\x1b")
	if err := command.Wait(); err != nil {
		t.Fatalf("agentsctl exit: %v\n%s", err, output.String())
	}
	<-readDone
	before, err := os.ReadFile(filepath.Join(fakeDir, "before.stty"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(fakeDir, "after.stty"))
	if err != nil {
		t.Fatal(err)
	}
	if normalizeStty(string(before)) != normalizeStty(string(after)) {
		t.Fatalf("terminal mode was not restored: before=%q after=%q", before, after)
	}
	stopTestDaemon(stateDir)
}

func normalizeStty(value string) string {
	fields := strings.Split(strings.TrimSpace(value), ":")
	for i, field := range fields {
		if !strings.HasPrefix(field, "lflag=") {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(field, "lflag="), 16, 64)
		if err == nil {
			fields[i] = fmt.Sprintf("lflag=%x", flags&^uint64(unix.PENDIN))
		}
	}
	return strings.Join(fields, ":")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func writeCatalogFixture(root, current string, count int) error {
	rows := make([]map[string]any, 0, count+2)
	for i := 0; i < count; i++ {
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("session-%03d", i), "name": fmt.Sprintf("session-%03d", i),
			"preview": "Ready", "cwd": current, "createdAt": i + 1, "updatedAt": i + 1,
			"status": map[string]any{"type": "idle"}, "archived": false,
		})
	}
	rows = append(rows,
		map[string]any{"id": "sibling", "name": "MUST-NOT-SHOW-SIBLING", "cwd": filepath.Join(filepath.Dir(current), "sibling"), "createdAt": count + 2, "updatedAt": count + 2, "status": map[string]any{"type": "idle"}, "archived": false},
		map[string]any{"id": "archived", "name": "MUST-NOT-SHOW-ARCHIVED", "cwd": current, "createdAt": count + 3, "updatedAt": count + 3, "status": map[string]any{"type": "idle"}, "archived": true},
	)
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "codex.json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "claude.json"), []byte("[]"), 0o600)
}

func environmentForTest(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func writePTY(t *testing.T, terminal *os.File, value string) {
	t.Helper()
	if _, err := terminal.WriteString(value); err != nil {
		t.Fatal(err)
	}
}

func waitOutput(output *lockedBuffer, text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q\n%s", text, output.String())
}

func waitOutputAfter(output *lockedBuffer, text string, minimumCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(output.String(), text) >= minimumCount {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for occurrence %d of %q\n%s", minimumCount, text, output.String())
}

func latestFrame(output string) string {
	const clear = "\x1b[2J\x1b[H"
	if index := strings.LastIndex(output, clear); index >= 0 {
		output = output[index+len(clear):]
	}
	return output
}

func waitLatestFrame(output *lockedBuffer, timeout time.Duration, predicate func(string) bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate(latestFrame(output.String())) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timed out waiting for final screen state")
}

func frameLines(frame string) []string {
	lines := strings.Split(frame, "\r\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func assertScreen(t *testing.T, output string, width, height int, required ...string) {
	t.Helper()
	lines := emulateANSIScreen(output, width, height)
	frame := strings.Join(lines, "\n")
	if len(lines) != height {
		t.Fatalf("screen lines=%d want=%d\n%s", len(lines), height, frame)
	}
	for _, line := range lines {
		cells := 0
		for _, r := range line {
			cells += runeCells(r)
		}
		if cells > width {
			t.Fatalf("screen row exceeds width %d: %q", width, line)
		}
		// Row layout is "> [status] title...": the title always starts at
		// cell 4 (cursor, separator, status, separator) regardless of
		// provider — there is no runner-glyph column, and provider is
		// conveyed only by the title's color, which emulateANSIScreen has
		// already stripped down to visible glyphs by this point.
		if cells := cellIndex(line, "session-"); cells >= 0 && cells != 4 {
			t.Fatalf("session row did not start at the shared column: %q", line)
		}
	}
	for _, text := range required {
		if !strings.Contains(frame, text) {
			t.Fatalf("screen missing %q:\n%s", text, frame)
		}
	}
	for _, forbidden := range []string{"MUST-NOT-SHOW-SIBLING", "MUST-NOT-SHOW-ARCHIVED"} {
		if strings.Contains(frame, forbidden) {
			t.Fatalf("screen contains %q:\n%s", forbidden, frame)
		}
	}
}

// cellIndex returns the terminal-cell offset of substr's first occurrence
// in line, or -1 if absent. Byte/rune offsets are not cell offsets once a
// line mixes multi-byte icon glyphs, so alignment checks must go through
// this instead of strings.Index.
func cellIndex(line, substr string) int {
	byteIdx := strings.Index(line, substr)
	if byteIdx < 0 {
		return -1
	}
	cells := 0
	for _, r := range line[:byteIdx] {
		cells += runeCells(r)
	}
	return cells
}

func emulateANSIScreen(output string, width, height int) []string {
	screen := make([][]rune, height)
	for row := range screen {
		screen[row] = make([]rune, width)
	}
	row, col := 0, 0
	for i := 0; i < len(output); {
		if output[i] == 0x1b && i+1 < len(output) && output[i+1] == '[' {
			j := i + 2
			for j < len(output) && !(output[j] >= 0x40 && output[j] <= 0x7e) {
				j++
			}
			if j >= len(output) {
				break
			}
			params, final := output[i+2:j], output[j]
			switch final {
			case 'J':
				if params == "2" {
					for y := range screen {
						clear(screen[y])
					}
				}
			case 'H':
				row, col = 0, 0
			}
			i = j + 1
			continue
		}
		switch output[i] {
		case '\r':
			col = 0
			i++
			continue
		case '\n':
			row++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(output[i:])
		if size == 0 {
			break
		}
		if r >= 0x20 && r != 0x7f && row >= 0 && row < height && col >= 0 && col < width {
			screen[row][col] = r
			col += runeCells(r)
		}
		i += size
	}
	lines := make([]string, height)
	for y := range screen {
		end := len(screen[y])
		for end > 0 && screen[y][end-1] == 0 {
			end--
		}
		line := append([]rune(nil), screen[y][:end]...)
		for i := range line {
			if line[i] == 0 {
				line[i] = ' '
			}
		}
		lines[y] = string(line)
	}
	return lines
}

func waitRunningRun(t *testing.T, path string) state.Run {
	t.Helper()
	store := state.New(path)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := store.Load()
		for _, run := range data.Runs {
			if run.State == "running" && run.PID > 0 {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("managed child did not reach running state")
	return state.Run{}
}

func waitRunStopped(t *testing.T, path, id string) {
	t.Helper()
	store := state.New(path)
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := store.Load()
		if run, ok := data.Runs[id]; ok && run.State == "stopped" && run.PID == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("managed child did not stop")
}

func waitArchived(t *testing.T, path, id string, output *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path)
		var rows []map[string]any
		_ = json.Unmarshal(b, &rows)
		for _, row := range rows {
			if row["id"] == id && row["archived"] == true {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("session was not archived; catalog=%s\nscreen=%s", b, latestFrame(output.String()))
}

func stopTestDaemon(stateDir string) {
	client := supervisor.Client{Socket: filepath.Join(stateDir, "supervisor.sock")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Call(ctx, supervisor.Request{Action: "ping"})
	if err == nil && response.DaemonPID > 0 {
		_ = syscall.Kill(response.DaemonPID, syscall.SIGTERM)
	}
}
