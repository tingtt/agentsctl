package session

import (
	"path/filepath"
	"strings"
)

// DirectoryScope selects which sessions' CWDs Agent View shows, relative to
// the directory agentsctl was started in. Cycled cwd -> cwd/** -> all ->
// cwd (see the DesignDoc's Directory scope section).
type DirectoryScope int

const (
	// ScopeCWD shows only sessions whose CWD is exactly the directory
	// agentsctl was started in.
	ScopeCWD DirectoryScope = iota
	// ScopeSubtree shows sessions whose CWD is the starting directory
	// itself or any descendant of it (an inclusive recursive subtree).
	ScopeSubtree
	// ScopeAll shows every session regardless of CWD.
	ScopeAll
)

// Scope is a Filter input: the directory-scope mode plus the starting
// directory it is relative to.
type Scope struct {
	CurrentDirectory string
	Directory        DirectoryScope
}

// Filter returns the subset of sessions visible under scope, preserving
// input order (callers apply SortOverview separately). It performs no I/O:
// CurrentDirectory and each Session's CWD are compared as given.
func Filter(sessions []Session, scope Scope) []Session {
	switch scope.Directory {
	case ScopeAll:
		return sessions
	case ScopeSubtree:
		current := normalizeDirectory(scope.CurrentDirectory)
		filtered := make([]Session, 0, len(sessions))
		for _, row := range sessions {
			if isWithinSubtree(current, row.CWD) {
				filtered = append(filtered, row)
			}
		}
		return filtered
	default: // ScopeCWD
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
