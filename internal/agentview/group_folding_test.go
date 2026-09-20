package agentview

import (
	"fmt"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

func foldingRows(count int, cwd string, pinned bool) []session.Session {
	rows := make([]session.Session, count)
	for i := range rows {
		rows[i] = session.Session{
			Key:       key(fmt.Sprintf("%s-%02d", cwd, i+1)),
			Name:      fmt.Sprintf("session %d", i+1),
			CWD:       cwd,
			Pinned:    pinned,
			CreatedAt: time.Unix(int64(count-i), 0),
		}
	}
	return rows
}

func countListItems(model selectableList, kind listItemKind) int {
	count := 0
	for _, item := range model.items {
		if item.id.kind == kind {
			count++
		}
	}
	return count
}

func TestDirectoryGroupsInitiallyShowTenSessionBlocks(t *testing.T) {
	tests := []struct {
		count        int
		wantSessions int
		wantMore     int
	}{
		{count: 0, wantSessions: 0, wantMore: 0},
		{count: 1, wantSessions: 1, wantMore: 0},
		{count: 10, wantSessions: 10, wantMore: 0},
		{count: 11, wantSessions: 10, wantMore: 1},
		{count: 20, wantSessions: 10, wantMore: 1},
		{count: 21, wantSessions: 10, wantMore: 1},
		{count: 35, wantSessions: 10, wantMore: 1},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_sessions", tt.count), func(t *testing.T) {
			s := NewState()
			s.SetRows(foldingRows(tt.count, "/work/repo", false))
			model := s.selectableList()
			if got := countListItems(model, listItemSession); got != tt.wantSessions {
				t.Fatalf("session items=%d, want %d", got, tt.wantSessions)
			}
			if got := countListItems(model, listItemShowMore); got != tt.wantMore {
				t.Fatalf("Show more items=%d, want %d", got, tt.wantMore)
			}
		})
	}
}

func TestDirectoryPaginationUsesSameIdentityForRecentlyCreatedAndPathHeading(t *testing.T) {
	rows := foldingRows(11, "/work/repo-a", false)
	s := NewState()
	s.SetRows(rows)
	single := s.selectableList()
	if len(single.groups) != 1 || single.groups[0].title != "Recently created" {
		t.Fatalf("single-directory groups=%+v", single.groups)
	}
	wantID := single.groups[0].id

	rows = append(rows, foldingRows(1, "/work/repo-b", false)...)
	s.SetRows(rows)
	multi := s.selectableList()
	if len(multi.groups) != 2 || multi.groups[0].title != displayCWD("/work/repo-a") {
		t.Fatalf("multi-directory groups=%+v", multi.groups)
	}
	if multi.groups[0].id != wantID {
		t.Fatalf("same directory changed identity: single=%+v multi=%+v", wantID, multi.groups[0].id)
	}
	if got := countListItems(multi, listItemShowMore); got != 1 {
		t.Fatalf("Show more items=%d, want 1", got)
	}
}

func TestPinnedNeverPaginates(t *testing.T) {
	s := NewState()
	s.SetRows(foldingRows(21, "/work/repo", true))
	model := s.selectableList()
	if got := countListItems(model, listItemSession); got != 21 {
		t.Fatalf("pinned session items=%d, want 21", got)
	}
	if got := countListItems(model, listItemShowMore); got != 0 {
		t.Fatalf("Pinned must not contain Show more: got %d", got)
	}
}
