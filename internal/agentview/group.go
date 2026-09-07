package agentview

import (
	"path/filepath"

	"github.com/tingtt/agentsctl/internal/session"
)

// rowGroup is one heading section of the rendered session list: a title,
// whether its rows carry an inline CWD column, and the indices into the
// caller's row slice it covers, in display order.
type rowGroup struct {
	title   string
	showCWD bool
	indices []int
}

// groupRows partitions rows (already sorted by session.SortOverview:
// pinned first, then newest-first) into the sections #14's list rendering
// requires:
//
//   - Pinned sessions always form a single "Pinned" group, regardless of
//     how many directories they span.
//   - Unpinned sessions form one "Recently created" group when every row
//     shares one directory (see multiDirectory), or one group per distinct
//     directory -- each headed by that directory's display path -- when
//     rows span more than one.
//
// Only the Pinned group's rows carry an inline CWD column, and only when
// rows span more than one directory: a row's own group heading already
// says its directory otherwise (see the DesignDoc's Session list section).
// groupRows performs no I/O; it only reads Pinned/CWD off the rows it's
// given and preserves their relative order within each group (selection
// stays keyed by session.Key regardless of how rows are regrouped -- see
// State.SelectedIndex).
func groupRows(rows []session.Session) []rowGroup {
	multi := multiDirectory(rows)
	var groups []rowGroup

	var pinned []int
	for i, r := range rows {
		if r.Pinned {
			pinned = append(pinned, i)
		}
	}
	if len(pinned) > 0 {
		groups = append(groups, rowGroup{title: "Pinned", showCWD: multi, indices: pinned})
	}

	if multi {
		var order []string
		buckets := map[string][]int{}
		for i, r := range rows {
			if r.Pinned {
				continue
			}
			k := directoryKey(r.CWD)
			if _, ok := buckets[k]; !ok {
				order = append(order, k)
			}
			buckets[k] = append(buckets[k], i)
		}
		for _, k := range order {
			groups = append(groups, rowGroup{title: displayCWD(k), indices: buckets[k]})
		}
		return groups
	}

	var unpinned []int
	for i, r := range rows {
		if !r.Pinned {
			unpinned = append(unpinned, i)
		}
	}
	if len(unpinned) > 0 {
		groups = append(groups, rowGroup{title: "Recently created", indices: unpinned})
	}
	return groups
}

// multiDirectory reports whether rows span more than one distinct
// directory -- the condition that switches list rendering from
// same-directory (no per-row CWD, "Recently created") to multi-directory
// (per-directory unpinned groups, directory path shown on Pinned rows).
func multiDirectory(rows []session.Session) bool {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[directoryKey(r.CWD)] = true
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

func directoryKey(path string) string { return filepath.Clean(path) }

// visualRowIndices flattens groupRows' output into the single ordered list
// of selectable row indices the list actually renders top-to-bottom --
// group headings and blank separators contribute nothing, since groupRows
// never puts them in a group's own indices. This is the one source render
// (View) and selection navigation (update.go's moveSelection) share for
// "row order as seen on screen", so a raw State.Rows index and its on-
// screen neighbor can never disagree.
func visualRowIndices(rows []session.Session) []int {
	var indices []int
	for _, g := range groupRows(rows) {
		indices = append(indices, g.indices...)
	}
	return indices
}
