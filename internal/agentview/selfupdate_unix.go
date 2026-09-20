//go:build darwin || linux

package agentview

import (
	"context"
	"errors"

	"github.com/tingtt/agentsctl/internal/selfupdate"
)

// Updater is the self-update boundary Runtime drives (implemented by
// selfupdate.Updater): the release lookup and the installation, both of which
// perform I/O and are therefore only ever called from a background goroutine.
type Updater interface {
	// Check reports the newer release, if any.
	Check(ctx context.Context) (a selfupdate.Availability, ok bool, err error)
	// Install installs exactly version and returns the installed executable.
	Install(ctx context.Context, version string) (executable string, err error)
}

// Restart asks the caller of Runtime.Run to replace the process with
// Executable once Run has returned, i.e. after the terminal has been restored.
// Agent View itself never replaces the process: it does not own that
// decision, and doing so while it holds raw mode would leave the terminal
// unusable.
type Restart struct {
	Executable string
}

// updateCheckEvent is the startup update check's result, carried over
// Runtime.checkCh from its background goroutine to the event loop, the only
// place State changes.
type updateCheckEvent struct {
	availability selfupdate.Availability
	ok           bool
	err          error
}

// updateInstallEvent is one installation's result, carried over
// Runtime.installCh.
type updateInstallEvent struct {
	executable string
	err        error
}

// Restart reports whether Run ended because an update was installed, and the
// executable to replace the process with. Only meaningful after Run returned
// a nil error.
func (r *Runtime) Restart() (Restart, bool) {
	if r.restart == nil {
		return Restart{}, false
	}
	return *r.restart, true
}

// updateContext returns the context bounding every self-update goroutine,
// created on first use from ctx and cancelled by stopUpdate when Run returns,
// so neither a release lookup nor a go install outlives Agent View.
func (r *Runtime) updateContext(ctx context.Context) context.Context {
	if r.updateCtx == nil {
		r.updateCtx, r.updateCancel = context.WithCancel(ctx)
	}
	return r.updateCtx
}

func (r *Runtime) stopUpdate() {
	if r.updateCancel != nil {
		r.updateCancel()
	}
}

// startUpdateCheck starts the single startup release check in the background
// and returns immediately. Without an Updater (a development build) it does
// nothing.
func (r *Runtime) startUpdateCheck(ctx context.Context) {
	if r.Updater == nil {
		return
	}
	if r.checkCh == nil {
		r.checkCh = make(chan updateCheckEvent, 1)
	}
	updater, ch, ctx := r.Updater, r.checkCh, r.updateContext(ctx)
	go func() {
		a, ok, err := updater.Check(ctx)
		ch <- updateCheckEvent{availability: a, ok: ok, err: err}
	}()
}

// applyUpdateCheck applies a check result to State. A failed check, or the
// absence of a newer release, changes nothing: it is never surfaced as an
// error.
func (r *Runtime) applyUpdateCheck(ev updateCheckEvent) {
	if ev.err == nil && ev.ok {
		r.State.ApplyUpdateAvailable(ev.availability)
	}
}

// startUpdateInstall starts installing version in the background and returns
// immediately, so the event loop stays responsive for the length of a go
// install. State.Updating rejects a second concurrent installation.
func (r *Runtime) startUpdateInstall(ctx context.Context, version string) error {
	if r.Updater == nil {
		return errors.New("update is unavailable in this build")
	}
	if r.State.Updating {
		return errors.New("update already in progress")
	}
	if r.installCh == nil {
		r.installCh = make(chan updateInstallEvent, 1)
	}
	r.State.Updating = true
	r.State.Composer.Clear()
	updater, ch, ctx := r.Updater, r.installCh, r.updateContext(ctx)
	go func() {
		executable, err := updater.Install(ctx, version)
		ch <- updateInstallEvent{executable: executable, err: err}
	}()
	return nil
}

// applyUpdateInstall applies an installation result on the event loop and
// reports whether Run should end for a restart. A failed installation leaves
// this process running and surfaces the failure in the error area; a
// successful one only records the Restart request: the process is replaced by
// Run's caller after the terminal is restored.
func (r *Runtime) applyUpdateInstall(ev updateInstallEvent) (restart bool) {
	r.State.Updating = false
	if ev.err != nil {
		r.State.Error = "error: update failed: " + ev.err.Error()
		return false
	}
	r.restart = &Restart{Executable: ev.executable}
	return true
}
