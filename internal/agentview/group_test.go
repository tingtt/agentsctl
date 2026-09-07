package agentview

import (
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func rowAt(k session.Key, cwd string, pinned bool) session.Session {
	return session.Session{Key: k, CWD: cwd, Pinned: pinned}
}

// TestGroupRowsSameDirectoryUsesRecentlyCreatedWithoutCWD fixes #14's
// same-directory rendering: a single "Recently created" group for unpinned
// rows, with no inline CWD column.
func TestGroupRowsSameDirectoryUsesRecentlyCreatedWithoutCWD(t *testing.T) {
	rows := []session.Session{
		rowAt(key("a"), "/work", false),
		rowAt(key("b"), "/work", false),
	}
	groups := groupRows(rows)
	if len(groups) != 1 {
		t.Fatalf("groups=%+v, want exactly one group", groups)
	}
	if groups[0].title != "Recently created" {
		t.Fatalf("title=%q, want %q", groups[0].title, "Recently created")
	}
	if groups[0].showCWD {
		t.Fatal("same-directory group must not show CWD")
	}
	if len(groups[0].indices) != 2 {
		t.Fatalf("indices=%v, want both rows", groups[0].indices)
	}
}

// TestGroupRowsSameDirectoryPinnedAlsoHidesCWD fixes that a Pinned group
// hides CWD too when every row -- pinned or not -- shares one directory.
func TestGroupRowsSameDirectoryPinnedAlsoHidesCWD(t *testing.T) {
	rows := []session.Session{
		rowAt(key("a"), "/work", true),
		rowAt(key("b"), "/work", false),
	}
	groups := groupRows(rows)
	if len(groups) != 2 {
		t.Fatalf("groups=%+v, want Pinned + Recently created", groups)
	}
	if groups[0].title != "Pinned" || groups[0].showCWD {
		t.Fatalf("pinned group=%+v, want Pinned with showCWD=false", groups[0])
	}
	if groups[1].title != "Recently created" {
		t.Fatalf("unpinned group title=%q, want %q", groups[1].title, "Recently created")
	}
}

// TestGroupRowsMultiDirectoryGroupsUnpinnedByDirectory fixes #14's
// multi-directory rendering: unpinned sessions form one group per distinct
// directory, each headed by that directory's display path.
func TestGroupRowsMultiDirectoryGroupsUnpinnedByDirectory(t *testing.T) {
	rows := []session.Session{
		rowAt(key("a"), "/work/repo-a", false),
		rowAt(key("b"), "/work/repo-a", false),
		rowAt(key("c"), "/work/repo-b", false),
	}
	groups := groupRows(rows)
	if len(groups) != 2 {
		t.Fatalf("groups=%+v, want one group per directory", groups)
	}
	if groups[0].title != displayCWD("/work/repo-a") || len(groups[0].indices) != 2 {
		t.Fatalf("group0=%+v", groups[0])
	}
	if groups[1].title != displayCWD("/work/repo-b") || len(groups[1].indices) != 1 {
		t.Fatalf("group1=%+v", groups[1])
	}
	if groups[0].showCWD || groups[1].showCWD {
		t.Fatal("a directory-headed unpinned group must not also show a per-row CWD")
	}
}

// TestGroupRowsMultiDirectoryPinnedIsSingleGroupWithCWD fixes that Pinned
// sessions collapse into one group across directories, with the CWD column
// shown per row since the heading itself doesn't disambiguate directory.
func TestGroupRowsMultiDirectoryPinnedIsSingleGroupWithCWD(t *testing.T) {
	rows := []session.Session{
		rowAt(key("a"), "/work/repo-a", true),
		rowAt(key("b"), "/work/repo-b", true),
		rowAt(key("c"), "/work/repo-a", false),
	}
	groups := groupRows(rows)
	if len(groups) != 2 {
		t.Fatalf("groups=%+v, want Pinned + one directory group", groups)
	}
	if groups[0].title != "Pinned" || !groups[0].showCWD || len(groups[0].indices) != 2 {
		t.Fatalf("pinned group=%+v, want Pinned showCWD=true with 2 rows", groups[0])
	}
}

// TestGroupRowsPreservesRowOrderWithinGroup fixes that grouping never
// reorders rows -- it only partitions the already-sorted
// (session.SortOverview) input, so newest-first ordering survives inside
// each group.
func TestGroupRowsPreservesRowOrderWithinGroup(t *testing.T) {
	rows := []session.Session{
		rowAt(key("newest"), "/work/repo-a", false),
		rowAt(key("older"), "/work/repo-a", false),
	}
	groups := groupRows(rows)
	if len(groups) != 1 || len(groups[0].indices) != 2 {
		t.Fatalf("groups=%+v", groups)
	}
	if groups[0].indices[0] != 0 || groups[0].indices[1] != 1 {
		t.Fatalf("indices=%v, want [0 1] (input order preserved)", groups[0].indices)
	}
}

// TestGroupRowsEmptyRows fixes that an empty catalog produces no groups at
// all (no stray "Recently created" heading over nothing).
func TestGroupRowsEmptyRows(t *testing.T) {
	if groups := groupRows(nil); len(groups) != 0 {
		t.Fatalf("groups=%+v, want none", groups)
	}
}

func TestMultiDirectoryDetection(t *testing.T) {
	if multiDirectory([]session.Session{rowAt(key("a"), "/work", false), rowAt(key("b"), "/work", true)}) {
		t.Fatal("identical directories must not count as multi-directory")
	}
	if !multiDirectory([]session.Session{rowAt(key("a"), "/work/a", false), rowAt(key("b"), "/work/b", false)}) {
		t.Fatal("distinct directories must count as multi-directory")
	}
}
