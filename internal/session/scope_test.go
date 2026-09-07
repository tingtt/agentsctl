package session

import "testing"

func sessionAt(cwd string) Session { return Session{CWD: cwd} }

func TestFilterScopeSameExactMatchOnly(t *testing.T) {
	sessions := []Session{sessionAt("/project"), sessionAt("/project/src"), sessionAt("/project-other")}
	got := Filter(sessions, Scope{CurrentDirectory: "/project", Directory: ScopeSame})
	if len(got) != 1 || got[0].CWD != "/project" {
		t.Fatalf("ScopeSame must match only the exact directory: %+v", got)
	}
}

func TestFilterScopeDescendantsRespectsPathBoundary(t *testing.T) {
	// A prefix-string match would wrongly include "/project-other" since it
	// starts with "/project" as a string -- see the DesignDoc's Path
	// matching section, which requires real path-boundary semantics.
	sessions := []Session{sessionAt("/project"), sessionAt("/project/src"), sessionAt("/project-other")}
	got := Filter(sessions, Scope{CurrentDirectory: "/project", Directory: ScopeDescendants})
	if len(got) != 2 {
		t.Fatalf("ScopeDescendants must include root+descendants but not the sibling: %+v", got)
	}
	for _, s := range got {
		if s.CWD == "/project-other" {
			t.Fatalf("ScopeDescendants must not treat a sibling directory sharing a string prefix as a descendant: %+v", got)
		}
	}
}

func TestFilterScopeDescendantsIncludesWorktreeDirectories(t *testing.T) {
	sessions := []Session{
		sessionAt("/project"),
		sessionAt("/project/src"),
		sessionAt("/worktrees/project-feature"),
		sessionAt("/worktrees/project-feature/src"),
		sessionAt("/other/unrelated-repo"),
	}
	got := Filter(sessions, Scope{
		CurrentDirectory:    "/project",
		Directory:           ScopeDescendants,
		WorktreeDirectories: []string{"/worktrees/project-feature"},
	})
	if len(got) != 4 {
		t.Fatalf("ScopeDescendants must include the worktree directory and its descendants: %+v", got)
	}
	for _, s := range got {
		if s.CWD == "/other/unrelated-repo" {
			t.Fatalf("ScopeDescendants must not include a session from an unrelated repository: %+v", got)
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

func TestFilterScopeSameDoesNotResolveSymlinks(t *testing.T) {
	// normalizeDirectory only filepath.Clean's the path; it must not stat
	// the filesystem or resolve symlinks (see the DesignDoc's Symlink
	// section), so a non-existent path is still compared logically.
	sessions := []Session{sessionAt("/does/not/exist/../exist")}
	got := Filter(sessions, Scope{CurrentDirectory: "/does/not/exist", Directory: ScopeSame})
	if len(got) != 1 {
		t.Fatalf("logical path cleaning must match without touching the filesystem: %+v", got)
	}
}
