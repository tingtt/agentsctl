package selfupdate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeDoer struct {
	calls int
	do    func(*http.Request) (*http.Response, error)
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls++
	return f.do(req)
}

func respond(status int, body string) *fakeDoer {
	return &fakeDoer{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
}

func release(tag string) string { return `{"tag_name":"` + tag + `"}` }

func goFound(string) (string, error)   { return "/fake/go", nil }
func goMissing(string) (string, error) { return "", errors.New("not found") }

func TestCheckComparesVersions(t *testing.T) {
	tests := []struct {
		name, current, latest string
		want                  bool
	}{
		{"current equals latest", "v1.1.0", "v1.1.0", false},
		{"current is newer", "v1.2.0", "v1.1.0", false},
		{"current is older", "v1.0.0", "v1.1.0", true},
		{"numeric not lexical", "v1.9.0", "v1.10.0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := &Updater{Current: tc.current, HTTP: respond(200, release(tc.latest)), LookPath: goFound}
			a, ok, err := u.Check(context.Background())
			if err != nil || ok != tc.want {
				t.Fatalf("Check = %+v, %v, %v; want ok=%v", a, ok, err, tc.want)
			}
			if ok && (a.Current != tc.current || a.Latest != tc.latest || !a.GoAvailable) {
				t.Fatalf("Availability = %+v", a)
			}
		})
	}
}

func TestCheckReportsGoAvailability(t *testing.T) {
	u := &Updater{Current: "v1.0.0", HTTP: respond(200, release("v1.1.0")), LookPath: goMissing}
	a, ok, err := u.Check(context.Background())
	if err != nil || !ok || a.GoAvailable {
		t.Fatalf("Check = %+v, %v, %v; want newer release without Go", a, ok, err)
	}
}

func TestCheckDevelopmentBuildMakesNoRequest(t *testing.T) {
	doer := respond(200, release("v9.9.9"))
	u := &Updater{Current: "dev", HTTP: doer}
	if _, ok, err := u.Check(context.Background()); ok || err != nil || doer.calls != 0 {
		t.Fatalf("Check ok=%v err=%v requests=%d; want none", ok, err, doer.calls)
	}
	if New("dev") != nil {
		t.Fatal("New(dev) must not return an updater")
	}
	if New("v1.0.0") == nil {
		t.Fatal("New(release) must return an updater")
	}
}

func TestCheckFailuresAreErrors(t *testing.T) {
	tests := map[string]*fakeDoer{
		"non-2xx":        respond(500, release("v2.0.0")),
		"malformed JSON": respond(200, `{"tag_name":`),
		"invalid tag":    respond(200, release("latest")),
		"short tag":      respond(200, release("v2.0")),
		"prerelease tag": respond(200, release("v2.0.0-rc.1")),
		"build metadata": respond(200, release("v2.0.0+meta")),
		"draft":          respond(200, `{"tag_name":"v2.0.0","draft":true}`),
		"prerelease":     respond(200, `{"tag_name":"v2.0.0","prerelease":true}`),
		"request failure": {do: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network down")
		}},
	}
	for name, doer := range tests {
		t.Run(name, func(t *testing.T) {
			u := &Updater{Current: "v1.0.0", HTTP: doer, LookPath: goFound}
			if a, ok, err := u.Check(context.Background()); err == nil || ok {
				t.Fatalf("Check = %+v, %v, %v; want error", a, ok, err)
			}
		})
	}
}

func TestCheckInvalidRunningVersionMakesNoRequest(t *testing.T) {
	doer := respond(200, release("v2.0.0"))
	u := &Updater{Current: "not-semver", HTTP: doer}
	if _, _, err := u.Check(context.Background()); err == nil || doer.calls != 0 {
		t.Fatalf("err=%v requests=%d; want error without a request", err, doer.calls)
	}
}

func TestCheckPassesContextToRequest(t *testing.T) {
	started := make(chan struct{})
	doer := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	u := &Updater{Current: "v1.0.0", HTTP: doer}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := u.Check(ctx); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check did not return after cancellation")
	}
}

func TestCheckUsesLatestReleaseEndpoint(t *testing.T) {
	var got *http.Request
	doer := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(release("v1.0.0")))}, nil
	}}
	u := &Updater{Current: "v1.0.0", HTTP: doer}
	if _, _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.URL.String() != "https://api.github.com/repos/tingtt/agentsctl/releases/latest" {
		t.Fatalf("url = %s", got.URL)
	}
}

type runCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls      []runCall
	gobin      string
	gopath     string
	envErr     error
	installErr error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runCall{name, args})
	switch args[0] {
	case "env":
		if f.envErr != nil {
			return nil, f.envErr
		}
		return []byte(`{"GOBIN":"` + f.gobin + `","GOPATH":"` + f.gopath + `"}`), nil
	case "install":
		return nil, f.installErr
	}
	return nil, errors.New("unexpected command")
}

// installedBinary is a Stat fake that reports only wantDir/agentsctl as an
// existing regular file.
func installedBinary(t *testing.T, wantDir string) func(string) (os.FileInfo, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "agentsctl")
	if err := os.WriteFile(file, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return func(name string) (os.FileInfo, error) {
		if name != filepath.Join(wantDir, "agentsctl") {
			return nil, os.ErrNotExist
		}
		return os.Stat(file)
	}
}

func TestInstallPassesExactVersionToModuleAndLdflags(t *testing.T) {
	r := &fakeRunner{gobin: "/opt/gobin"}
	u := &Updater{Runner: r, LookPath: goFound, Stat: installedBinary(t, "/opt/gobin")}
	path, err := u.Install(context.Background(), "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/opt/gobin/agentsctl" {
		t.Fatalf("path = %q", path)
	}
	install := r.calls[len(r.calls)-1]
	want := []string{
		"install",
		"-ldflags=-X github.com/tingtt/agentsctl/internal/version.Version=v1.1.0",
		"github.com/tingtt/agentsctl/cmd/agentsctl@v1.1.0",
	}
	if install.name != "/fake/go" || !reflect.DeepEqual(install.args, want) {
		t.Fatalf("install = %s %q, want %q", install.name, install.args, want)
	}
	for _, c := range r.calls {
		for _, a := range c.args {
			if strings.Contains(a, "@latest") {
				t.Fatalf("@latest used: %q", c.args)
			}
		}
	}
}

func TestInstallDestinationResolution(t *testing.T) {
	tests := []struct {
		name, gobin, gopath, want string
	}{
		{"GOBIN wins", "/opt/gobin", "/a:/b", "/opt/gobin/agentsctl"},
		{"first GOPATH entry", "", "/a:/b", "/a/bin/agentsctl"},
		{"single GOPATH", "", "/home/u/go", "/home/u/go/bin/agentsctl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := &Updater{Runner: &fakeRunner{gobin: tc.gobin, gopath: tc.gopath}, LookPath: goFound, Stat: installedBinary(t, filepath.Dir(tc.want))}
			path, err := u.Install(context.Background(), "v1.1.0")
			if err != nil || path != tc.want {
				t.Fatalf("Install = %q, %v; want %q", path, err, tc.want)
			}
		})
	}
}

func TestInstallFailures(t *testing.T) {
	missing := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	tests := []struct {
		name string
		u    *Updater
	}{
		{"go unavailable", &Updater{Runner: &fakeRunner{}, LookPath: goMissing}},
		{"install fails", &Updater{Runner: &fakeRunner{gobin: "/g", installErr: errors.New("boom")}, LookPath: goFound}},
		{"go env fails", &Updater{Runner: &fakeRunner{envErr: errors.New("boom")}, LookPath: goFound}},
		{"no install directory", &Updater{Runner: &fakeRunner{}, LookPath: goFound}},
		{"binary missing after install", &Updater{Runner: &fakeRunner{gobin: "/g"}, LookPath: goFound, Stat: missing}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if path, err := tc.u.Install(context.Background(), "v1.1.0"); err == nil || path != "" {
				t.Fatalf("Install = %q, %v; want error and no path", path, err)
			}
		})
	}
}

func TestInstallRejectsInvalidVersionBeforeRunningAnything(t *testing.T) {
	r := &fakeRunner{gobin: "/g"}
	u := &Updater{Runner: r, LookPath: goFound}
	for _, v := range []string{"", "latest", "v1", "v1.2.3-rc.1", "v1.2.3+x", "v1.1.0 --evil"} {
		if _, err := u.Install(context.Background(), v); err == nil {
			t.Fatalf("Install(%q) succeeded", v)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("commands ran for invalid versions: %v", r.calls)
	}
}

// TestExecRunnerDoesNotUseShell pins that arguments reach the process
// verbatim: a shell would expand or split them.
func TestExecRunnerDoesNotUseShell(t *testing.T) {
	out, err := execRunner{}.Run(context.Background(), "echo", "$HOME;", "a b")
	if err != nil || string(out) != "$HOME; a b\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestExecRunnerErrorIncludesStderr(t *testing.T) {
	_, err := execRunner{}.Run(context.Background(), "sh", "-c", "echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("err = %v", err)
	}
}
