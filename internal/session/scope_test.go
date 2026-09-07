package session

import "testing"

func sessionAt(cwd string) Session { return Session{CWD: cwd} }

func TestFilterScopeCWDExactMatchOnly(t *testing.T) {
	sessions := []Session{sessionAt("/project"), sessionAt("/project/src"), sessionAt("/project-other")}
	got := Filter(sessions, Scope{CurrentDirectory: "/project", Directory: ScopeCWD})
	if len(got) != 1 || got[0].CWD != "/project" {
		t.Fatalf("ScopeCWD must match only the exact directory: %+v", got)
	}
}

func TestFilterScopeSubtreeRespectsPathBoundary(t *testing.T) {
	// A prefix-string match would wrongly include "/project-other" since it
	// starts with "/project" as a string -- see the DesignDoc's Path
	// matching section, which requires real path-boundary semantics.
	sessions := []Session{sessionAt("/project"), sessionAt("/project/src"), sessionAt("/project-other")}
	got := Filter(sessions, Scope{CurrentDirectory: "/project", Directory: ScopeSubtree})
	if len(got) != 2 {
		t.Fatalf("ScopeSubtree must include root+descendants but not the sibling: %+v", got)
	}
	for _, s := range got {
		if s.CWD == "/project-other" {
			t.Fatalf("ScopeSubtree must not treat a sibling directory sharing a string prefix as a descendant: %+v", got)
		}
	}
}

func TestFilterScopeAllIncludesEverything(t *testing.T) {
	sessions := []Session{sessionAt("/a"), sessionAt("/b")}
	got := Filter(sessions, Scope{CurrentDirectory: "/a", Directory: ScopeAll})
	if len(got) != 2 {
		t.Fatalf("ScopeAll must return every session: %+v", got)
	}
}

func TestFilterScopeCWDDoesNotResolveSymlinks(t *testing.T) {
	// normalizeDirectory only filepath.Clean's the path; it must not stat
	// the filesystem or resolve symlinks (see the DesignDoc's Symlink
	// section), so a non-existent path is still compared logically.
	sessions := []Session{sessionAt("/does/not/exist/../exist")}
	got := Filter(sessions, Scope{CurrentDirectory: "/does/not/exist", Directory: ScopeCWD})
	if len(got) != 1 {
		t.Fatalf("logical path cleaning must match without touching the filesystem: %+v", got)
	}
}
