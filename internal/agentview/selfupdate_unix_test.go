//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/selfupdate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// checkCall is one fakeUpdater.Check invocation, held open until the test
// replies (or the context ends).
type checkCall struct {
	reply chan checkReply
}

type checkReply struct {
	availability selfupdate.Availability
	ok           bool
	err          error
}

// installCall is one fakeUpdater.Install invocation, held open until the test
// replies (or the context ends).
type installCall struct {
	version string
	ctx     context.Context
	reply   chan installReply
}

type installReply struct {
	executable string
	err        error
}

// fakeUpdater is an Updater whose calls the test drives one at a time, so no
// test ever touches GitHub or a Go installation.
type fakeUpdater struct {
	checks   chan checkCall
	installs chan installCall
}

func newFakeUpdater() *fakeUpdater {
	return &fakeUpdater{checks: make(chan checkCall, 4), installs: make(chan installCall, 4)}
}

func (f *fakeUpdater) Check(ctx context.Context) (selfupdate.Availability, bool, error) {
	call := checkCall{reply: make(chan checkReply, 1)}
	f.checks <- call
	select {
	case r := <-call.reply:
		return r.availability, r.ok, r.err
	case <-ctx.Done():
		return selfupdate.Availability{}, false, ctx.Err()
	}
}

func (f *fakeUpdater) Install(ctx context.Context, version string) (string, error) {
	call := installCall{version: version, ctx: ctx, reply: make(chan installReply, 1)}
	f.installs <- call
	select {
	case r := <-call.reply:
		return r.executable, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func awaitCheck(t *testing.T, f *fakeUpdater) checkCall {
	t.Helper()
	select {
	case c := <-f.checks:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("update check was never started")
		return checkCall{}
	}
}

func awaitInstall(t *testing.T, f *fakeUpdater) installCall {
	t.Helper()
	select {
	case c := <-f.installs:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("update install was never started")
		return installCall{}
	}
}

// dispatchCounter counts provider Dispatch calls: a /update prompt reaching
// it is the bug these tests guard against.
type dispatchCounter struct {
	*fakeProvider
	dispatched atomic.Int32
}

func (d *dispatchCounter) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	d.dispatched.Add(1)
	return d.fakeProvider.Dispatch(ctx, prompt, cwd)
}

// updateHarness is a Runtime driven through its real eventLoop with fake key
// input and a fake Updater.
type updateHarness struct {
	rt       *Runtime
	updater  *fakeUpdater
	provider *dispatchCounter
	out      *syncBuffer
	keys     chan keyResult
	done     chan error
}

func newUpdateHarness(t *testing.T) *updateHarness {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inR.Close(); inW.Close() })
	h := &updateHarness{
		updater:  newFakeUpdater(),
		provider: &dispatchCounter{fakeProvider: &fakeProvider{id: session.ProviderClaude}},
		out:      &syncBuffer{},
		keys:     make(chan keyResult),
		done:     make(chan error, 1),
	}
	h.rt = &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{h.provider}, Pins: &fakePins{}},
		State:      NewState(),
		Input:      inR,
		Output:     h.out,
		CWD:        "/work",
		Updater:    h.updater,
	}
	return h
}

// start mirrors Run's startup sequence -- initial reload, first frame, then
// the background update check -- before entering the event loop.
func (h *updateHarness) start(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.rt.requestReload(ctx)
	h.rt.render()
	h.rt.startUpdateCheck(ctx)
	go func() {
		h.done <- h.rt.eventLoop(ctx, bufio.NewReader(h.rt.Input), fakeReadKey(h.keys))
	}()
	t.Cleanup(cancel)
	return cancel
}

func (h *updateHarness) press(ev KeyEvent) { h.keys <- keyResult{ev: keyInput(ev)} }

func (h *updateHarness) typeText(text string) {
	for _, r := range text {
		h.press(KeyEvent{Key: KeyRune, Rune: r})
	}
}

func (h *updateHarness) submit(text string) {
	h.typeText(text)
	h.press(KeyEvent{Key: KeyEnter})
}

func (h *updateHarness) waitForFrame(t *testing.T, want string) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(latestFrame(h.out.String()), want) })
}

func (h *updateHarness) finish(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("eventLoop did not exit")
		return nil
	}
}

func (h *updateHarness) quit(t *testing.T) {
	t.Helper()
	h.press(KeyEvent{Key: KeyEsc})
	if err := h.finish(t); err != nil {
		t.Fatal(err)
	}
}

var newerRelease = checkReply{availability: selfupdate.Availability{Current: "v1.0.0", Latest: "v1.1.0", GoAvailable: true}, ok: true}

func TestStartupCheckDoesNotBlockRenderingOrInput(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	check := awaitCheck(t, h.updater) // in flight, GitHub "never" answers yet

	h.typeText("H")
	h.waitForFrame(t, "H")

	// The result reaches State only through the event loop.
	check.reply <- newerRelease
	h.waitForFrame(t, "! agentsctl update available (v1.0.0 -> v1.1.0): /update to update")
	h.press(KeyEvent{Key: KeyEsc}) // clears the prompt
	h.quit(t)
}

func TestFailedOrEmptyCheckShowsNothing(t *testing.T) {
	for name, reply := range map[string]checkReply{
		"failure":       {err: errors.New("network down")},
		"nothing newer": {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newUpdateHarness(t)
			h.start(t)
			awaitCheck(t, h.updater).reply <- reply

			h.typeText("H") // a later frame proves the reply was already applied
			h.waitForFrame(t, "H")
			if frame := latestFrame(h.out.String()); strings.Contains(frame, "! ") || strings.Contains(frame, "update") {
				t.Fatalf("frame shows an update-check outcome:\n%s", frame)
			}
			h.press(KeyEvent{Key: KeyEsc}) // clears the prompt
			h.quit(t)
		})
	}
}

func TestNoUpdaterStartsNoCheck(t *testing.T) {
	rt := &Runtime{State: NewState()}
	rt.startUpdateCheck(context.Background())
	if rt.checkCh != nil || rt.updateCtx != nil {
		t.Fatal("a Runtime without an Updater must not start an update check")
	}
	// /update in such a build is still local, and reports the absence.
	s := NewState()
	if intent := submit(&s, "/update"); intent.Kind != IntentNone || s.Error == "" {
		t.Fatalf("intent=%+v error=%q", intent, s.Error)
	}
}

func TestUpdateNeverReachesProviderDispatch(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	awaitCheck(t, h.updater).reply <- checkReply{} // nothing newer known

	h.submit("/update")
	h.waitForFrame(t, "! No update available")
	h.press(KeyEvent{Key: KeyEsc})
	h.submit("/update now")
	h.waitForFrame(t, "! /update takes no arguments")
	h.press(KeyEvent{Key: KeyEsc})
	h.quit(t)

	if n := h.provider.dispatched.Load(); n != 0 {
		t.Fatalf("provider Dispatch called %d times for /update", n)
	}
	select {
	case c := <-h.updater.installs:
		t.Fatalf("unexpected install of %s", c.version)
	default:
	}
}

func TestUpdateInstallsAdvertisedVersionWithoutBlockingTheLoop(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	awaitCheck(t, h.updater).reply <- newerRelease

	h.submit("/update")
	install := awaitInstall(t, h.updater)
	if install.version != "v1.1.0" {
		t.Fatalf("installed %q, want the advertised v1.1.0", install.version)
	}
	h.waitForFrame(t, "! updating agentsctl (v1.0.0 -> v1.1.0)...")

	// go install is still running: the loop keeps taking input.
	h.typeText("H")
	h.waitForFrame(t, "H")

	// A second /update must not start a concurrent installation.
	h.press(KeyEvent{Key: KeyEsc})
	h.submit("/update")
	h.waitForFrame(t, "! Update already in progress")
	select {
	case c := <-h.updater.installs:
		t.Fatalf("duplicate install of %s started", c.version)
	case <-time.After(50 * time.Millisecond):
	}

	// A failed installation keeps the process running and reports the error.
	install.reply <- installReply{err: errors.New("go install: boom")}
	h.waitForFrame(t, "! error: update failed: go install: boom")
	h.press(KeyEvent{Key: KeyEsc})
	h.quit(t)
	if _, ok := h.rt.Restart(); ok {
		t.Fatal("a failed installation must not request a restart")
	}
	if n := h.provider.dispatched.Load(); n != 0 {
		t.Fatalf("provider Dispatch called %d times", n)
	}
}

func TestUpdateFailureKeepsNoticeAndAllowsRetry(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	awaitCheck(t, h.updater).reply <- newerRelease
	h.waitForFrame(t, "/update to update")

	h.submit("/update")
	awaitInstall(t, h.updater).reply <- installReply{err: errors.New("boom")}
	h.waitForFrame(t, "! error: update failed: boom")

	// The next successfully dispatched intent clears the error, and the
	// update notice is visible again.
	h.press(KeyEvent{Key: KeyCtrlL})
	h.waitForFrame(t, "! agentsctl update available (v1.0.0 -> v1.1.0): /update to update")

	h.submit("/update")
	awaitInstall(t, h.updater).reply <- installReply{err: errors.New("boom again")} // Updating was reset, so a retry is allowed
	h.waitForFrame(t, "! error: update failed: boom again")
	h.quit(t)
}

// TestSuccessfulInstallEndsEventLoopWithRestartRequest fixes that the
// background goroutine only reports the result: the loop itself ends and
// hands the restart to its caller.
func TestSuccessfulInstallEndsEventLoopWithRestartRequest(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	awaitCheck(t, h.updater).reply <- newerRelease
	h.waitForFrame(t, "/update to update")

	h.submit("/update")
	install := awaitInstall(t, h.updater)
	if _, ok := h.rt.Restart(); ok {
		t.Fatal("restart requested before the installation finished")
	}
	install.reply <- installReply{executable: "/go/bin/agentsctl"}

	if err := h.finish(t); err != nil {
		t.Fatal(err)
	}
	restart, ok := h.rt.Restart()
	if !ok || restart.Executable != "/go/bin/agentsctl" {
		t.Fatalf("Restart() = %+v, %v", restart, ok)
	}
}

// TestRunRestoresTerminalBeforeReturningRestartRequest runs the real Run on a
// PTY: by the time Run returns the restart request, raw mode and the
// alternate screen are already released, so exec by the caller can never
// inherit an Agent View-owned terminal.
func TestRunRestoresTerminalBeforeReturningRestartRequest(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	original := terminalMode(t, slave)

	keys := make(chan keyResult)
	defer close(keys)
	out := &syncBuffer{}
	updater := newFakeUpdater()
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{&fakeProvider{id: session.ProviderClaude}}, Pins: &fakePins{}},
		State:      NewState(),
		Input:      slave,
		Output:     out,
		CWD:        "/work",
		Updater:    updater,
		ReadInput:  fakeReadKey(keys),
	}
	runDone := make(chan error, 1)
	go func() { runDone <- rt.Run(context.Background()) }()

	awaitCheck(t, updater).reply <- newerRelease
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(latestFrame(out.String()), "/update to update") })
	if terminalMode(t, slave) == original {
		t.Fatal("Agent View should own the terminal while running")
	}
	for _, r := range "/update" {
		keys <- keyResult{ev: keyInput(KeyEvent{Key: KeyRune, Rune: r})}
	}
	keys <- keyResult{ev: keyInput(KeyEvent{Key: KeyEnter})}
	awaitInstall(t, updater).reply <- installReply{executable: "/go/bin/agentsctl"}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a successful install")
	}
	if restart, ok := rt.Restart(); !ok || restart.Executable != "/go/bin/agentsctl" {
		t.Fatalf("Restart() = %+v, %v", restart, ok)
	}
	if got := terminalMode(t, slave); got != original {
		t.Fatalf("terminal mode after Run = %q, want the original %q", got, original)
	}
	if !strings.HasSuffix(out.String(), "\x1b[0m\x1b[?25h\x1b[?1049l") {
		t.Fatalf("alternate screen not released last; output tail: %q", out.String()[max(0, len(out.String())-40):])
	}
}

func TestStopUpdateCancelsInFlightInstall(t *testing.T) {
	h := newUpdateHarness(t)
	h.start(t)
	awaitCheck(t, h.updater).reply <- newerRelease
	h.submit("/update")
	install := awaitInstall(t, h.updater)

	h.rt.stopUpdate() // what Run does on return
	select {
	case <-install.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight install was not cancelled")
	}
	h.waitForFrame(t, "! error: update failed: context canceled")
	h.quit(t)
}
