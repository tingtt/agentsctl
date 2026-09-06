package tui

import (
	"bufio"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

// TestRenameDoesNotInvokeProviderList mirrors
// TestPinDoesNotInvokeProviderList: a successful rename is applied directly
// to the Model's existing row (Model.ApplyRename) using the name
// Provider.Rename already confirmed against the native catalog, so it must
// not trigger any provider's List() -- unlike every other action besides
// Pin/unpin.
func TestRenameDoesNotInvokeProviderList(t *testing.T) {
	key := session.Key{Provider: session.ProviderClaude, ID: "c"}
	claude := &journeyProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: key, Name: "old", CWD: "/work", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}},
	}}
	codex := &journeyProvider{id: session.ProviderCodex}
	m := NewModel()
	m.Rows = append([]session.Session(nil), claude.rows...)
	m.Selected = 0
	app := App{Catalog: session.Catalog{Providers: []session.Provider{claude, codex}}, Model: m, CWD: "/work"}

	app.Model.Update("rename")
	app.Model.RenameDraft = "new"
	action := app.Model.Update("enter")
	if action.Kind != ActionRename {
		t.Fatalf("action=%+v", action)
	}
	if err := app.act(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if claude.ListCallCount() != 0 || codex.ListCallCount() != 0 {
		t.Fatalf("rename invoked provider List: claude=%d codex=%d", claude.ListCallCount(), codex.ListCallCount())
	}
	if app.Model.Rows[0].Name != "new" {
		t.Fatalf("rename was not applied to the model row: %+v", app.Model.Rows[0])
	}
	if app.Model.Renaming {
		t.Fatal("a successful rename must exit rename mode")
	}
}

// TestRenameFrameUpdateDoesNotWaitForSlowProviderList is the rename
// counterpart to TestPinFrameUpdateDoesNotWaitForSlowProviderList: it
// drives the real App.Run event loop over a pty, with both providers'
// List() calls carrying an injected delay standing in for a slow
// `claude`/`codex` CLI subprocess, and asserts the frame rendered right
// after Enter confirms the rename reflects the new name well before that
// delay could have elapsed -- proving Run's post-action refresh is skipped
// for a successful rename (see app_unix.go's Run loop), not just that
// Provider.Rename itself happens to be fast.
func TestRenameFrameUpdateDoesNotWaitForSlowProviderList(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	const delay = 400 * time.Millisecond
	key := session.Key{Provider: session.ProviderClaude, ID: "c"}
	// Name starts empty so the simulated keystrokes below ("n","e","w",...)
	// compose the whole target name directly, with no need to first clear
	// a pre-filled draft (entering rename mode seeds RenameDraft from the
	// row's current name).
	claude := &journeyProvider{id: session.ProviderClaude, listDelay: delay, rows: []session.Session{
		{Key: key, Name: "", CWD: "/work", CreatedAt: time.Now(), Capabilities: session.Capabilities{Rename: true}},
	}}
	codex := &journeyProvider{id: session.ProviderCodex, listDelay: delay}
	catalog := session.Catalog{Providers: []session.Provider{claude, codex}, Pins: store}

	var output bytes.Buffer
	var writeTimes []time.Time
	trackedOutput := writerFunc(func(p []byte) (int, error) {
		writeTimes = append(writeTimes, time.Now())
		return output.Write(p)
	})

	var readTimes []time.Time
	keys := []string{"rename", "n", "e", "w", "-", "n", "a", "m", "e", "enter", "quit"}
	idx := 0
	readInput := func(*bufio.Reader) (string, error) {
		k := keys[idx]
		idx++
		readTimes = append(readTimes, time.Now())
		return k, nil
	}

	model := NewModel()
	model.Scope = session.ScopeAll
	app := App{Catalog: catalog, Model: model, Input: slave, Output: trackedOutput, CWD: "/work", ReadInput: readInput}
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Per iteration the loop renders (write), then reads one key: write[0]
	// is beginTerminal's own pre-loop write, write[1] is the first
	// in-loop render (before any key is read), and write[i+2] is the
	// render reflecting read[i]'s effect (see
	// TestPinFrameUpdateDoesNotWaitForSlowProviderList's identical
	// indexing). "enter" is the second-to-last key.
	enterReadIndex := len(keys) - 2
	enterWriteIndex := enterReadIndex + 2
	if len(writeTimes) <= enterWriteIndex || len(readTimes) <= enterReadIndex {
		t.Fatalf("insufficient frames/reads captured: writes=%d reads=%d", len(writeTimes), len(readTimes))
	}
	elapsed := writeTimes[enterWriteIndex].Sub(readTimes[enterReadIndex])
	t.Logf("rename Enter -> updated frame: %v (injected provider List delay: %v)", elapsed, delay)
	if elapsed > delay/2 {
		t.Fatalf("rename frame update took %v after Enter, wanted well under the injected %v provider List delay (proves it does not wait on provider refresh)", elapsed, delay)
	}
	if !strings.Contains(latestFrame(output.String()), "new-name") {
		t.Fatalf("rendered output never reflected the rename:\n%s", output.String())
	}
	if claude.ListCallCount() != 1 || codex.ListCallCount() != 1 {
		t.Fatalf("rename triggered an extra provider List beyond the initial load: claude=%d codex=%d", claude.ListCallCount(), codex.ListCallCount())
	}
}
