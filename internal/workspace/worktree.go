// Package workspace is the git/filesystem discovery boundary for Agent
// View's ScopeDescendants directory scope: it finds the worktree
// directories belonging to the repository containing a given directory, so
// internal/session's pure Filter (see session.Scope.WorktreeDirectories)
// never has to shell out to git itself.
//
//	git/filesystem discovery (this package)
//	        -> normalized scope roots / worktree dirs
//	        -> pure session filtering (internal/session)
package workspace

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// Worktrees returns the working directories of every git worktree
// belonging to the repository that contains dir (including dir's own
// worktree root), via `git worktree list --porcelain` run with dir as the
// working directory.
//
// Worktrees is best-effort: if dir is not inside a git repository, git is
// unavailable, or the command otherwise fails, it returns nil rather than
// an error -- ScopeDescendants must still work (falling back to just dir's
// own subtree) when worktree discovery isn't possible, the same way a
// provider catalog failure never blocks the other provider's sessions (see
// the DesignDoc's Catalog loading section).
func Worktrees(ctx context.Context, dir string) []string {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	return parseWorktreeList(out.String())
}

// parseWorktreeList extracts each "worktree <path>" line's path from `git
// worktree list --porcelain` output -- the pure parsing step, kept separate
// from Worktrees' process I/O so it can be unit-tested against fixture
// output without a real git repository.
func parseWorktreeList(output string) []string {
	var dirs []string
	for _, line := range strings.Split(output, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && path != "" {
			dirs = append(dirs, path)
		}
	}
	return dirs
}
