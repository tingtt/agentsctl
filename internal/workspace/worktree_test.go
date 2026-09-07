package workspace

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestParseWorktreeListExtractsPaths is a pure, fixture-based test of the
// parsing step alone -- no git process involved -- covering the exact
// `git worktree list --porcelain` shape (one "worktree <path>" line per
// entry, interleaved with HEAD/branch/bare/detached lines this parser must
// ignore).
func TestParseWorktreeListExtractsPaths(t *testing.T) {
	output := "worktree /project\n" +
		"HEAD abc123\n" +
		"branch refs/heads/main\n" +
		"\n" +
		"worktree /worktrees/project-feature\n" +
		"HEAD def456\n" +
		"branch refs/heads/feature\n"
	got := parseWorktreeList(output)
	want := []string{"/project", "/worktrees/project-feature"}
	if len(got) != len(want) {
		t.Fatalf("parseWorktreeList() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseWorktreeList() = %v, want %v", got, want)
		}
	}
}

func TestParseWorktreeListEmptyOutput(t *testing.T) {
	if got := parseWorktreeList(""); got != nil {
		t.Fatalf("parseWorktreeList(\"\") = %v, want nil", got)
	}
}

// TestWorktreesDiscoversRealGitWorktrees exercises the real git process
// boundary end to end: an actual repository with a real linked worktree,
// verifying Worktrees reports both the main and the linked worktree
// directories.
func TestWorktreesDiscoversRealGitWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := t.TempDir()
	run(t, main, "init")
	run(t, main, "commit", "--allow-empty", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, main, "worktree", "add", "-b", "feature", linked)

	got := Worktrees(context.Background(), main)
	if !containsPath(t, got, main) {
		t.Fatalf("Worktrees(%q) = %v, want it to include the main worktree", main, got)
	}
	if !containsPath(t, got, linked) {
		t.Fatalf("Worktrees(%q) = %v, want it to include the linked worktree %q", main, got, linked)
	}
}

func TestWorktreesNotAGitRepositoryReturnsNil(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if got := Worktrees(context.Background(), dir); got != nil {
		t.Fatalf("Worktrees(non-repo) = %v, want nil", got)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func containsPath(t *testing.T, dirs []string, target string) bool {
	t.Helper()
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		resolvedTarget = target
	}
	for _, d := range dirs {
		resolved, err := filepath.EvalSymlinks(d)
		if err != nil {
			resolved = d
		}
		if resolved == resolvedTarget {
			return true
		}
	}
	return false
}
