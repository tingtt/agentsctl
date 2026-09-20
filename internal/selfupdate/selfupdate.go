// Package selfupdate is agentsctl's self-update boundary: it owns every
// network, process and filesystem operation an update needs -- looking up the
// latest GitHub Release, comparing semantic versions, detecting the Go
// toolchain, running "go install" and locating the binary it wrote -- so that
// Agent View only sees an Availability and an installed executable path.
//
// The running version comes from internal/version (an ldflags-injected value);
// this package never consults Go build info. Only Unix is supported.
package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/tingtt/agentsctl/internal/version"
)

const (
	// ReleasesURL is the human-facing release page shown when automatic
	// update is unavailable.
	ReleasesURL = "https://github.com/tingtt/agentsctl/releases"

	latestReleaseURL = "https://api.github.com/repos/tingtt/agentsctl/releases/latest"
	modulePath       = "github.com/tingtt/agentsctl"
	commandPath      = modulePath + "/cmd/agentsctl"
	versionSymbol    = modulePath + "/internal/version.Version"
	binaryName       = "agentsctl"

	checkTimeout = 10 * time.Second
	// maxReleaseBody bounds the release JSON read from the network.
	maxReleaseBody = 1 << 20
)

// Availability describes a release newer than the running version.
type Availability struct {
	// Current is the running version, Latest the newer release tag. Latest
	// is a validated semantic version and is the exact value Install must
	// be given.
	Current, Latest string
	// GoAvailable reports whether a go executable was found, i.e. whether
	// Install can be attempted.
	GoAvailable bool
}

// Runner runs an external command without a shell and returns its standard
// output. A failing command's error carries its standard error.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// HTTPDoer is the subset of *http.Client used to fetch the release.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Updater checks for and installs newer agentsctl releases. The zero value
// of every dependency selects the real implementation; tests inject fakes so
// nothing touches GitHub or the developer's Go installation.
type Updater struct {
	// Current is the running release version (internal/version.Version).
	Current string

	HTTP     HTTPDoer
	Runner   Runner
	LookPath func(file string) (string, error)
	// Stat verifies the installed executable exists.
	Stat func(name string) (os.FileInfo, error)
	// ReleaseURL overrides the GitHub latest-release API endpoint.
	ReleaseURL string
}

// New returns an Updater for the running version, or nil when that is a
// development build: such a build never performs an update check.
func New(current string) *Updater {
	if current == version.Development {
		return nil
	}
	return &Updater{Current: current}
}

// Check reports whether a release newer than the running version exists. ok
// is false, with a nil error, when the running version is current or newer
// (or is a development build, in which case no request is made). Every
// failure -- transport, status, malformed JSON, invalid version -- is
// returned as an error for the caller to treat as non-fatal.
func (u *Updater) Check(ctx context.Context) (a Availability, ok bool, err error) {
	if u.Current == version.Development {
		return Availability{}, false, nil
	}
	if !semver.IsValid(u.Current) {
		return Availability{}, false, fmt.Errorf("running version %q is not a semantic version", u.Current)
	}
	latest, err := u.latestRelease(ctx)
	if err != nil {
		return Availability{}, false, err
	}
	if semver.Compare(latest, u.Current) <= 0 {
		return Availability{}, false, nil
	}
	_, goErr := u.lookPath()("go")
	return Availability{Current: u.Current, Latest: latest, GoAvailable: goErr == nil}, true, nil
}

type releaseResponse struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func (u *Updater) latestRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	url := u.ReleaseURL
	if url == "" {
		url = latestReleaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agentsctl")
	doer := u.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}
	resp, err := doer.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("latest release request returned %s", resp.Status)
	}
	var release releaseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&release); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	if release.Draft || release.Prerelease {
		return "", errors.New("latest release is a draft or prerelease")
	}
	if err := validateVersion(release.TagName); err != nil {
		return "", err
	}
	return release.TagName, nil
}

// validateVersion accepts only a stable, canonical semantic version tag such
// as "v1.2.3". The value ends up in a go install argument, so anything else
// (build metadata, a short "v1.2" form, prerelease, arbitrary text) is
// rejected.
func validateVersion(v string) error {
	if !semver.IsValid(v) || semver.Canonical(v) != v || semver.Prerelease(v) != "" {
		return fmt.Errorf("release tag %q is not a stable semantic version", v)
	}
	return nil
}

// Install installs exactly the given release version with go install,
// injecting the same version into the binary, and returns the path of the
// installed executable (Go's install destination: GOBIN, otherwise the first
// GOPATH entry's bin directory -- not whatever "agentsctl" on PATH resolves
// to). The version must be one Check returned.
func (u *Updater) Install(ctx context.Context, release string) (string, error) {
	if err := validateVersion(release); err != nil {
		return "", err
	}
	goPath, err := u.lookPath()("go")
	if err != nil {
		return "", errors.New("go executable not found")
	}
	dest, err := u.installPath(ctx, goPath)
	if err != nil {
		return "", err
	}
	if _, err := u.runner().Run(ctx, goPath, installArgs(release)...); err != nil {
		return "", fmt.Errorf("go install: %w", err)
	}
	stat := u.Stat
	if stat == nil {
		stat = os.Stat
	}
	if info, err := stat(dest); err != nil || info.IsDir() {
		return "", fmt.Errorf("installed executable not found at %s", dest)
	}
	return dest, nil
}

// installArgs is the go install argument vector for release. The version is
// used for both the module version and the -X linker flag so the running
// binary reports exactly what was installed.
func installArgs(release string) []string {
	return []string{
		"install",
		"-ldflags=-X " + versionSymbol + "=" + release,
		commandPath + "@" + release,
	}
}

// installPath resolves where go install writes the binary.
func (u *Updater) installPath(ctx context.Context, goPath string) (string, error) {
	out, err := u.runner().Run(ctx, goPath, "env", "-json", "GOBIN", "GOPATH")
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	var env struct{ GOBIN, GOPATH string }
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("decode go env: %w", err)
	}
	return installDir(env.GOBIN, env.GOPATH, binaryName)
}

func installDir(gobin, gopath, name string) (string, error) {
	if gobin != "" {
		return filepath.Join(gobin, name), nil
	}
	first := filepath.SplitList(gopath)
	if len(first) == 0 || first[0] == "" {
		return "", errors.New("cannot resolve go install directory: GOBIN and GOPATH are empty")
	}
	return filepath.Join(first[0], "bin", name), nil
}

func (u *Updater) runner() Runner {
	if u.Runner != nil {
		return u.Runner
	}
	return execRunner{}
}

func (u *Updater) lookPath() func(string) (string, error) {
	if u.LookPath != nil {
		return u.LookPath
	}
	return exec.LookPath
}

// execRunner runs commands directly through os/exec, never a shell.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
