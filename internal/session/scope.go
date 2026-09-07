package session

import (
	"path/filepath"
	"strings"
)

// DirectoryScope selects which sessions' CWDs Agent View shows, relative to
// the directory agentsctl was started in. Cycled same directory ->
// descendants + worktree directories -> all -> same directory (see the
// DesignDoc's Directory scope section).
type DirectoryScope int

const (
	// ScopeSame shows only sessions whose CWD is exactly the directory
	// agentsctl was started in.
	ScopeSame DirectoryScope = iota
	// ScopeDescendants shows sessions whose CWD is the starting directory
	// itself, a descendant of it (an inclusive recursive subtree), or
	// itself/a descendant of one of Scope.WorktreeDirectories -- the
	// working directories of git worktrees belonging to the same
	// repository as the starting directory.
	ScopeDescendants
	// ScopeAll shows every session regardless of CWD.
	ScopeAll
)

// Scope is a Filter input: the directory-scope mode, the starting directory
// it is relative to, and (for ScopeDescendants) any additional worktree
// roots to include alongside it.
type Scope struct {
	CurrentDirectory string
	Directory        DirectoryScope
	// WorktreeDirectories are extra ScopeDescendants roots -- each included
	// together with its own descendants, exactly like CurrentDirectory --
	// discovered outside this package (see internal/workspace) since
	// finding them requires git/filesystem I/O. Filter never discovers
	// these itself; it only ever compares the paths it is given. Ignored
	// for ScopeSame and ScopeAll.
	WorktreeDirectories []string
}

// Filter returns the subset of sessions visible under scope, preserving
// input order (callers apply SortOverview separately). It performs no I/O:
// CurrentDirectory, WorktreeDirectories, and each Session's CWD are
// compared as given.
func Filter(sessions []Session, scope Scope) []Session {
	switch scope.Directory {
	case ScopeAll:
		return sessions
	case ScopeDescendants:
		roots := make([]string, 0, 1+len(scope.WorktreeDirectories))
		roots = append(roots, scope.CurrentDirectory)
		roots = append(roots, scope.WorktreeDirectories...)
		filtered := make([]Session, 0, len(sessions))
		for _, row := range sessions {
			if isWithinAnySubtree(roots, row.CWD) {
				filtered = append(filtered, row)
			}
		}
		return filtered
	default: // ScopeSame
		current := normalizeDirectory(scope.CurrentDirectory)
		filtered := make([]Session, 0, len(sessions))
		for _, row := range sessions {
			if normalizeDirectory(row.CWD) == current {
				filtered = append(filtered, row)
			}
		}
		return filtered
	}
}

// normalizeDirectory intentionally does not resolve symlinks: providers
// compare filepath-cleaned logical paths and agentsctl keeps that UX
// contract (see the DesignDoc's Symlink section).
func normalizeDirectory(path string) string { return filepath.Clean(path) }

// isWithinAnySubtree reports whether candidate is within (or is) any of
// roots.
func isWithinAnySubtree(roots []string, candidate string) bool {
	for _, root := range roots {
		if isWithinSubtree(root, candidate) {
			return true
		}
	}
	return false
}

// isWithinSubtree reports whether candidate is root itself or a descendant
// of root, using filepath.Rel on cleaned logical paths (no symlink
// resolution, matching normalizeDirectory) rather than strings.HasPrefix --
// a prefix match alone cannot tell "/foo/bar" apart from the sibling
// "/foo/bar-other", since the latter starts with the former as a string
// but not as a path (see the DesignDoc's Path matching section).
func isWithinSubtree(root, candidate string) bool {
	root = normalizeDirectory(root)
	candidate = normalizeDirectory(candidate)
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
