package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// fakeForeground records foreground client runs instead of running them.
// A nil err stands for both a clean client exit and a Ctrl+] detach, which
// ForegroundClient reports alike.
type fakeForeground struct {
	calls   int
	path    string
	args    []string
	cwd     string
	in      *os.File
	out     io.Writer
	err     error
	onStart func()
}

func (f *fakeForeground) Run(_ context.Context, path string, args []string, cwd string, in *os.File, out io.Writer) error {
	f.calls++
	f.path, f.args, f.cwd, f.in, f.out = path, slices.Clone(args), cwd, in, out
	if f.onStart != nil {
		f.onStart()
	}
	return f.err
}

// newOpenProvider returns a Provider whose daemon lifecycle reports d's
// socket, with a scripted writer probe and a recording foreground launcher.
func newOpenProvider(t *testing.T, d *fakeDaemon) (*Provider, *scriptedLifecycle, *writerProbe, *fakeForeground) {
	t.Helper()
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{readyDaemon(d.socket)}}
	probe := &writerProbe{writers: map[string]bool{}, calls: map[string]int{}}
	fg := &fakeForeground{}
	p := &Provider{
		API:        &fakeAPI{},
		Daemon:     lifecycle,
		Foreground: fg,
		writerFree: probe.free,
	}
	return p, lifecycle, probe, fg
}

func threadSession(id string) session.Session {
	return session.Session{Key: codexKey(id), CWD: "/work/" + id}
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRemoteResumeArgsPassSocketPathVerbatim(t *testing.T) {
	got := remoteResumeArgs("/tmp/codex.sock", "thread-1")
	want := []string{"--remote", "unix:///tmp/codex.sock", "resume", "thread-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if got := remoteResumeArgs("/tmp/a b%20.sock", "t"); got[1] != "unix:///tmp/a b%20.sock" {
		t.Fatalf("socket path must not be encoded: %q", got[1])
	}
}

func TestOpenLaunchesRemoteResumeAfterEnsureAndPreflight(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("thread-1", 1))
	d.setLoaded("thread-1", idle)
	p, lifecycle, _, fg := newOpenProvider(t, d)
	fg.onStart = func() {
		if lifecycle.calls != 1 || d.callCount("thread/read") != 1 {
			t.Errorf("at launch: Ensure calls=%d thread/read calls=%d, want both done first", lifecycle.calls, d.callCount("thread/read"))
		}
	}
	in, out := devNull(t), &bytes.Buffer{}

	if err := p.Open(context.Background(), threadSession("thread-1"), in, out); err != nil {
		t.Fatal(err)
	}
	want := []string{"--remote", "unix://" + d.socket, "resume", "thread-1"}
	if fg.calls != 1 || fg.path != "codex" || !slices.Equal(fg.args, want) || fg.cwd != "/work/thread-1" {
		t.Fatalf("launch = %d x %s %q in %q, want codex %q in /work/thread-1", fg.calls, fg.path, fg.args, fg.cwd, want)
	}
	if fg.in != in || fg.out != io.Writer(out) {
		t.Fatalf("terminal = %v/%v, want the caller's", fg.in, fg.out)
	}

	fg.onStart = nil
	p.Path = "/opt/bin/codex"
	if err := p.Open(context.Background(), threadSession("thread-1"), in, out); err != nil {
		t.Fatal(err)
	}
	if fg.path != "/opt/bin/codex" {
		t.Fatalf("path = %q, want Provider.Path", fg.path)
	}
}

func TestOpenEnsureFailureLaunchesNothing(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("thread-1", 1))
	p, lifecycle, _, fg := newOpenProvider(t, d)
	lifecycle.results = []lifecycleResult{{err: errBoom}}

	err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Open = %v, want the Ensure failure", err)
	}
	if fg.calls != 0 || d.callCount("thread/read") != 0 {
		t.Fatalf("launches=%d thread/read=%d after Ensure failed", fg.calls, d.callCount("thread/read"))
	}
}

func TestOpenControlSocketOverrideBypassesDaemonEnsure(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("thread-1", 1))
	p, lifecycle, _, fg := newOpenProvider(t, d)
	p.ControlSocket = d.socket
	lifecycle.results = []lifecycleResult{{err: errors.New("real daemon must not be ensured")}}

	if err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard); err != nil {
		t.Fatal(err)
	}
	if lifecycle.calls != 0 || fg.calls != 1 || fg.args[1] != "unix://"+d.socket {
		t.Fatalf("Ensure calls=%d launches=%d args=%q", lifecycle.calls, fg.calls, fg.args)
	}
}

// A failed remote client is Open's failure: no second launch, nothing
// stopped. A successful exit or a detach stops nothing either.
func TestOpenReturnsRemoteFailureWithoutFallbackOrCleanup(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("thread-1", 1))
	d.setLoaded("thread-1", active())
	p, _, _, fg := newOpenProvider(t, d)
	fg.err = errBoom

	err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Open = %v, want the remote client's failure", err)
	}
	fg.err = nil
	if err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard); err != nil {
		t.Fatal(err)
	}
	if fg.calls != 2 || d.callCount("thread/start") != 0 {
		t.Fatalf("launches=%d thread/start=%d, want one launch per Open and nothing else", fg.calls, d.callCount("thread/start"))
	}
	for _, method := range []string{"turn/interrupt", "thread/unsubscribe"} {
		if n := d.callCount(method); n != 0 {
			t.Fatalf("%s called %d times on client exit", method, n)
		}
	}
}

func TestOpenRefusesRowWithoutThreadID(t *testing.T) {
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{readyDaemon("unused")}}
	fg := &fakeForeground{}
	p := &Provider{API: &fakeAPI{}, Daemon: lifecycle, Foreground: fg}
	row := session.Session{Key: session.Key{Provider: session.ProviderCodex}, Actions: session.Actions{session.ActionOpen: {Available: true}}}
	if err := p.Open(context.Background(), row, devNull(t), io.Discard); err == nil {
		t.Fatal("Open succeeded without a thread ID")
	}
	if lifecycle.calls != 0 || fg.calls != 0 {
		t.Fatalf("Ensure calls=%d launches=%d, want none", lifecycle.calls, fg.calls)
	}
}

// The daemon's own status decides; the writer lock matters only for a
// thread the daemon has not loaded.
func TestOpenPreflightWriterRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     ThreadStatus
		writerHeld bool
		launch     bool
		probes     int
	}{
		{name: "notLoaded writer free", status: notLoadedSt, launch: true, probes: 1},
		{name: "notLoaded writer held", status: notLoadedSt, writerHeld: true, probes: 1},
		{name: "active writer held", status: active(), writerHeld: true, launch: true},
		{name: "idle writer held", status: idle, writerHeld: true, launch: true},
		{name: "systemError writer held", status: ThreadStatus{Type: statusSystemError}, writerHeld: true, launch: true},
		{name: "future loaded status writer held", status: ThreadStatus{Type: "somethingNew"}, writerHeld: true, launch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.setThreads(catalogThread("thread-1", 1))
			d.setLoaded("thread-1", tc.status)
			p, _, probe, fg := newOpenProvider(t, d)
			probe.writers["thread-1"] = tc.writerHeld

			err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard)
			if tc.launch != (err == nil) || tc.launch != (fg.calls == 1) {
				t.Fatalf("Open = %v with %d launches, want launch=%v", err, fg.calls, tc.launch)
			}
			if !tc.launch && !errors.Is(err, errExternalWriter) {
				t.Fatalf("Open = %v, want the external-writer refusal", err)
			}
			if got := probe.callsFor("thread-1"); got != tc.probes {
				t.Fatalf("writer probes = %d, want %d", got, tc.probes)
			}
		})
	}
}

func TestOpenPreflightFailuresLaunchNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeDaemon, *Provider)
	}{
		{"dial", func(d *fakeDaemon, p *Provider) { p.ControlSocket = d.socket + ".missing" }},
		{"initialize", func(d *fakeDaemon, _ *Provider) { d.failNext("initialize", 1) }},
		{"thread/read", func(d *fakeDaemon, _ *Provider) { d.failNext("thread/read", 1) }},
		{"thread not found", func(d *fakeDaemon, _ *Provider) { d.setThreads() }},
		{"malformed", func(d *fakeDaemon, _ *Provider) {
			d.beforeRead = func(*fakeConn, string) *ThreadStatus { return &ThreadStatus{} }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.setThreads(catalogThread("thread-1", 1))
			p, _, _, fg := newOpenProvider(t, d)
			tc.setup(d, p)
			if err := p.Open(context.Background(), threadSession("thread-1"), devNull(t), io.Discard); err == nil {
				t.Fatal("Open succeeded without an established status")
			}
			if fg.calls != 0 {
				t.Fatalf("launches = %d, want none", fg.calls)
			}
		})
	}
}

func TestOpenRefusesTitleGenerationThread(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(titleThread("title-1"))
	d.setLoaded("title-1", idle)
	p, _, _, fg := newOpenProvider(t, d)
	if err := p.Open(context.Background(), threadSession("title-1"), devNull(t), io.Discard); err == nil {
		t.Fatal("Open launched an internal title-generation thread")
	}
	if fg.calls != 0 {
		t.Fatalf("launches = %d, want none", fg.calls)
	}
}

// A thread another process writes outside the shared daemon is offered
// none of Open, Stop or Archive.
func TestListExternalWriterOffersNoRuntimeActions(t *testing.T) {
	api := &fakeAPI{rows: []Thread{{ID: "thread-1", CWD: "/work", Status: notLoadedSt}}}
	p := &Provider{API: api, writerFree: func(string) bool { return false }}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Runtime != session.RuntimeExternal {
		t.Fatalf("rows = %+v, want one external thread", rows)
	}
	for _, action := range []session.ActionID{session.ActionOpen, session.ActionStop, session.ActionArchive} {
		if rows[0].Actions.Available(action) {
			t.Fatalf("%s offered for an external writer: %+v", action, rows[0].Actions)
		}
	}
}
