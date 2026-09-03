package pty

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/protocol"
)

func TestDetachIsConsumedAndNeverForwarded(t *testing.T) {
	input := bytes.NewReader([]byte{'a', DetachKey, 'b'})
	var sent bytes.Buffer
	detached := false
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { detached = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sent.String() != "a" || !detached {
		t.Fatalf("sent=%q detached=%v", sent.String(), detached)
	}
	if bytes.Contains(sent.Bytes(), []byte{DetachKey}) {
		t.Fatal("detach key reached child")
	}
}
func TestInputEOFIsReported(t *testing.T) {
	err := forwardInput(bytes.NewReader(nil), func([]byte) error { return nil }, func() error { return nil })
	if err != io.EOF {
		t.Fatalf("err=%v", err)
	}
}
// TestClaudeDetachUsesControlZWhenClientConsumesIt models the installed
// `claude attach` client: a well-behaved client puts its own PTY into raw
// mode (disabling ISIG so a literal 0x1a byte reaches its read loop instead
// of becoming a kernel-generated SIGTSTP) and exits on Ctrl+Z, mirroring the
// verified real CLI. detachClaudeClient must succeed via that byte alone,
// never reaching for a signal.
func TestClaudeDetachUsesControlZWhenClientConsumesIt(t *testing.T) {
	cmd := exec.Command("sh", "-c", "stty raw -echo; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0")
	child, err := creackpty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	go io.Copy(io.Discard, child) // production drains child output the same way; an unread PTY buffer can wedge a client mid-exit
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let `stty raw` land before the byte is sent
	if err := detachClaudeClient(context.Background(), cmd, child, wait, time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeDetachFallsBackToSignalsWhenClientIgnoresControlZ covers a client
// that never consumes the byte (e.g. stuck in a submode that owns input
// itself). detachClaudeClient must still end only the owned client process,
// via SIGHUP/SIGTERM, without hanging.
func TestClaudeDetachFallsBackToSignalsWhenClientIgnoresControlZ(t *testing.T) {
	// stty -isig keeps the kernel from turning the Ctrl+Z byte into a real
	// SIGTSTP job-control stop, so the fallback signal path is what's tested.
	cmd := exec.Command("sh", "-c", "stty -isig; trap 'exit 0' HUP TERM; while :; do sleep 1; done")
	child, err := creackpty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	go io.Copy(io.Discard, child) // production drains child output the same way; an unread PTY buffer can wedge a client mid-exit
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let `stty -isig` land before the byte is sent
	if err := detachClaudeClient(context.Background(), cmd, child, wait, time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestStartClaudeAttachRawDeliversByteSentDuringChildStartupWindow proves
// the fix for a real race found against the installed `claude` CLI:
// sending the detach byte (literal Ctrl+Z, 0x1a) immediately after attach
// starts -- before the child has gotten around to putting its own tty
// into raw mode -- used to be intercepted by the kernel's cooked-mode
// line discipline as the VSUSP special character (visibly echoed back as
// "^Z") and turned into SIGTSTP instead of ever reaching the app as
// input, silently swallowing the detach. startClaudeAttachRaw closes that
// window by putting the pty's slave side into raw mode before the child
// process starts. This models that exact window with a child that
// deliberately delays before touching its own terminal at all.
func TestStartClaudeAttachRawDeliversByteSentDuringChildStartupWindow(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "got")
	cmd := exec.Command("sh", "-c", "sleep 0.3; dd bs=1 count=1 of='"+outPath+"' 2>/dev/null")
	master, err := startClaudeAttachRaw(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	// Sent well before the child's 0.3s sleep elapses: squarely inside
	// the window a not-yet-raw pty would still be in cooked mode.
	if _, err := master.Write([]byte{0x1a}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 0x1a {
		t.Fatalf("byte sent during the child's startup window was not delivered as literal data: %v", got)
	}
}

func TestCtrlSIsForwardedToAttachedChild(t *testing.T) {
	input := bytes.NewReader([]byte{'a', 0x13, 'b', DetachKey})
	var sent bytes.Buffer
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent.Bytes(), []byte{'a', 0x13, 'b'}) {
		t.Fatalf("sent=%v", sent.Bytes())
	}
}

func TestInitialTerminalSizeIsARedrawResizeFrame(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := creackpty.Setsize(slave, &creackpty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	var frames bytes.Buffer
	stop := resizeTerminal(slave, &lockedFrames{w: &frames})
	stop()
	kind, payload, err := protocol.Read(&frames)
	if err != nil {
		t.Fatal(err)
	}
	var size protocol.TerminalSize
	if err := json.Unmarshal(payload, &size); err != nil {
		t.Fatal(err)
	}
	if kind != protocol.Resize || !size.Redraw || size.Rows != 24 || size.Cols != 80 {
		t.Fatalf("kind=%q size=%+v", kind, size)
	}
	if kind == protocol.Input {
		t.Fatal("terminal size sync was encoded as child input")
	}
}
