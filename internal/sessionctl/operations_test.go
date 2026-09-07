package sessionctl

import (
	"context"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestDispatchRoutesToProviderAndRequestsReload(t *testing.T) {
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderClaude}, dispatchResult: session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: "new"}}}
	c := Controller{Providers: []Source{p}}
	s, result, err := c.Dispatch(context.Background(), session.ProviderClaude, "hello", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if s.Key.ID != "new" || p.dispatchedArgs[0] != "hello" || p.dispatchedArgs[1] != "/cwd" {
		t.Fatalf("dispatch did not reach provider correctly: session=%+v args=%v", s, p.dispatchedArgs)
	}
	if !result.Reload {
		t.Fatal("Dispatch must request a reload so the new session appears")
	}
}

func TestDispatchFailsClosedWhenProviderIsNotADispatcher(t *testing.T) {
	c := Controller{Providers: []Source{&fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt"}}}}
	if _, _, err := c.Dispatch(context.Background(), "chatgpt", "hi", "/cwd"); err == nil {
		t.Fatal("Dispatch must fail closed for a provider that does not implement Dispatcher")
	}
}

func TestDispatchableReflectsCapabilityWithoutSwitchingOnProviderID(t *testing.T) {
	c := Controller{Providers: []Source{
		&fakeFullProvider{fakeSource: fakeSource{id: session.ProviderClaude}},
		&fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt"}},
	}}
	if !c.Dispatchable(session.ProviderClaude) {
		t.Fatal("a Dispatcher-implementing provider must be dispatchable")
	}
	if c.Dispatchable("chatgpt") {
		t.Fatal("a Source+Opener-only provider must not be dispatchable")
	}
	if c.Dispatchable("unknown") {
		t.Fatal("an unconfigured provider must not be dispatchable")
	}
}

func TestOpenRoutesToProviderAndRequestsReload(t *testing.T) {
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderCodex}}
	c := Controller{Providers: []Source{p}}
	key := session.Key{Provider: session.ProviderCodex, ID: "s1"}
	result, err := c.Open(context.Background(), session.Session{Key: key}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.opened) != 1 || p.opened[0] != key {
		t.Fatalf("Open did not reach provider: %+v", p.opened)
	}
	if !result.Reload {
		t.Fatal("Open must request a reload")
	}
}

func TestOpenFailsClosedWhenProviderIsNotAnOpener(t *testing.T) {
	c := Controller{Providers: []Source{fakeSource{id: session.ProviderClaude}}}
	if _, err := c.Open(context.Background(), session.Session{Key: session.Key{Provider: session.ProviderClaude}}, nil, nil); err == nil {
		t.Fatal("Open must fail closed for a Source-only provider")
	}
}

func TestStopRoutesToProviderAndRequestsReload(t *testing.T) {
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderCodex}}
	c := Controller{Providers: []Source{p}}
	key := session.Key{Provider: session.ProviderCodex, ID: "s1"}
	result, err := c.Stop(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.stopped) != 1 || p.stopped[0] != key || !result.Reload {
		t.Fatalf("stopped=%+v reload=%v", p.stopped, result.Reload)
	}
}

func TestStopFailsClosedWhenProviderIsNotAStopper(t *testing.T) {
	c := Controller{Providers: []Source{&fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt"}}}}
	if _, err := c.Stop(context.Background(), session.Key{Provider: "chatgpt"}); err == nil {
		t.Fatal("Stop must fail closed for a provider that does not implement Stopper")
	}
}

func TestArchiveFailsClosedWhenProviderIsNotAnArchiver(t *testing.T) {
	c := Controller{Providers: []Source{&fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt"}}}}
	if _, err := c.Archive(context.Background(), session.Key{Provider: "chatgpt"}); err == nil {
		t.Fatal("Archive must fail closed for a provider that does not implement Archiver")
	}
}

func TestArchiveRoutesToProviderAndRequestsReload(t *testing.T) {
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderClaude}}
	c := Controller{Providers: []Source{p}}
	key := session.Key{Provider: session.ProviderClaude, ID: "s1"}
	result, err := c.Archive(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.archived) != 1 || !result.Reload {
		t.Fatalf("archived=%+v reload=%v", p.archived, result.Reload)
	}
}

func TestRenameAppliesConfirmedNameAsPatchWithoutReload(t *testing.T) {
	// Rename's Result must never set Reload: the Renamer contract already
	// guarantees the name is confirmed by native state before returning
	// nil (see provider/claude), so the caller can apply it directly.
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderClaude}}
	c := Controller{Providers: []Source{p}}
	key := session.Key{Provider: session.ProviderClaude, ID: "s1"}
	result, err := c.Rename(context.Background(), key, "new name")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reload {
		t.Fatal("Rename must not request a reload")
	}
	if result.Patch == nil || result.Patch.Name == nil || *result.Patch.Name != "new name" || result.Patch.Key != key {
		t.Fatalf("Patch=%+v", result.Patch)
	}
	if p.renamed[key] != "new name" {
		t.Fatalf("rename did not reach provider: %+v", p.renamed)
	}
}

func TestRenameRejectsEmptyNameWithoutCallingProvider(t *testing.T) {
	p := &fakeFullProvider{fakeSource: fakeSource{id: session.ProviderClaude}}
	c := Controller{Providers: []Source{p}}
	if _, err := c.Rename(context.Background(), session.Key{Provider: session.ProviderClaude}, "   "); err == nil {
		t.Fatal("Rename must reject a blank name")
	}
	if p.renamed != nil {
		t.Fatal("Rename must not call the provider for a rejected name")
	}
}

func TestRenameFailsClosedWhenProviderIsNotARenamer(t *testing.T) {
	c := Controller{Providers: []Source{&fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt"}}}}
	if _, err := c.Rename(context.Background(), session.Key{Provider: "chatgpt"}, "x"); err == nil {
		t.Fatal("Rename must fail closed for a provider that does not implement Renamer")
	}
}

func TestTogglePinAppliesLocalPatchWithoutReload(t *testing.T) {
	c := Controller{Pins: &fakePinStore{}}
	key := session.Key{Provider: session.ProviderClaude, ID: "s1"}
	result, err := c.TogglePin(key)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reload {
		t.Fatal("Pin must never require a provider reload")
	}
	if result.Patch == nil || result.Patch.Pinned == nil || !*result.Patch.Pinned {
		t.Fatalf("Patch=%+v", result.Patch)
	}
}

func TestTogglePinFailsClosedWithoutAPinStore(t *testing.T) {
	c := Controller{}
	if _, err := c.TogglePin(session.Key{Provider: session.ProviderClaude, ID: "s1"}); err == nil {
		t.Fatal("TogglePin must fail without a configured PinStore")
	}
}
