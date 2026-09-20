package agentview

import (
	"path/filepath"

	"github.com/tingtt/agentsctl/internal/session"
)

// rowGroup is one heading section of the rendered session list: a title,
// whether its rows carry an inline CWD column, and the indices into the
// caller's row slice it covers, in display order.
type rowGroup struct {
	id      groupID
	title   string
	showCWD bool
	indices []int
}

type groupID struct {
	pinned    bool
	directory string
}

type groupDisplayState struct {
	folded       bool
	visibleCount int
}

type listItemKind uint8

const (
	listItemSession listItemKind = iota
	listItemShowMore
	listItemShowSessions
)

type listItemID struct {
	kind       listItemKind
	sessionKey session.Key
	group      groupID
}

type selectableItem struct {
	id       listItemID
	rowIndex int
}

type selectableGroup struct {
	rowGroup
	items []selectableItem
}

type selectableList struct {
	groups []selectableGroup
	items  []selectableItem
}

const directoryPageSize = 10

func sessionItemID(key session.Key) listItemID {
	return listItemID{kind: listItemSession, sessionKey: key}
}

func controlItemID(group groupID, kind listItemKind) listItemID {
	return listItemID{kind: kind, group: group}
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
// given and preserves their relative order within each group. Group identity
// uses the fixed Pinned identity or the same normalized directory key used for
// grouping, independently of the rendered heading.
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
		groups = append(groups, rowGroup{id: groupID{pinned: true}, title: "Pinned", showCWD: multi, indices: pinned})
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
			groups = append(groups, rowGroup{id: groupID{directory: k}, title: displayCWD(k), indices: buckets[k]})
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
		directory := directoryKey(rows[unpinned[0]].CWD)
		groups = append(groups, rowGroup{id: groupID{directory: directory}, title: "Recently created", indices: unpinned})
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

// deriveSelectableList is the single presentation model for list rendering,
// keyboard navigation, viewport tracking, and fold transitions. It contains
// real session references and Agent View-only control identities; headings and
// separators are deliberately absent from its selectable item sequence.
func deriveSelectableList(rows []session.Session, states map[groupID]groupDisplayState) selectableList {
	var model selectableList
	for _, rawGroup := range groupRows(rows) {
		group := selectableGroup{rowGroup: rawGroup}
		state := states[rawGroup.id]
		if state.folded {
			group.items = []selectableItem{{id: controlItemID(rawGroup.id, listItemShowSessions), rowIndex: -1}}
		} else {
			visible := len(rawGroup.indices)
			if !rawGroup.id.pinned {
				visible = state.visibleCount
				if visible <= 0 {
					visible = directoryPageSize
				}
				visible = min(visible, len(rawGroup.indices))
			}
			for _, rowIndex := range rawGroup.indices[:visible] {
				group.items = append(group.items, selectableItem{
					id:       sessionItemID(rows[rowIndex].Key),
					rowIndex: rowIndex,
				})
			}
			if visible < len(rawGroup.indices) {
				group.items = append(group.items, selectableItem{id: controlItemID(rawGroup.id, listItemShowMore), rowIndex: -1})
			}
		}
		model.groups = append(model.groups, group)
		model.items = append(model.items, group.items...)
	}
	return model
}

func (m selectableList) item(id listItemID) (selectableItem, bool) {
	for _, item := range m.items {
		if item.id == id {
			return item, true
		}
	}
	return selectableItem{}, false
}

func (m selectableList) groupForItem(id listItemID) (int, selectableGroup, bool) {
	for i, group := range m.groups {
		for _, item := range group.items {
			if item.id == id {
				return i, group, true
			}
		}
	}
	return -1, selectableGroup{}, false
}
