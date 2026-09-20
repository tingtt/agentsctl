package agentview

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

func focusControl(t *testing.T, s *State, kind listItemKind, group groupID) {
	t.Helper()
	id := controlItemID(group, kind)
	if _, ok := s.selectableList().item(id); !ok {
		t.Fatalf("control %+v is not selectable", id)
	}
	s.cursor, s.hasCursor = id, true
}

func requireSelectedSession(t *testing.T, s State, want session.Key) {
	t.Helper()
	row, ok := s.SelectedRow()
	if !ok || row.Key != want {
		t.Fatalf("selected row=%+v ok=%v, want %s", row.Key, ok, want)
	}
}

func requireControlCursor(t *testing.T, s State, kind listItemKind, group groupID) {
	t.Helper()
	want := controlItemID(group, kind)
	if !s.hasCursor || s.cursor != want {
		t.Fatalf("cursor=%+v hasCursor=%v, want %+v", s.cursor, s.hasCursor, want)
	}
	if _, ok := s.SelectedRow(); ok {
		t.Fatal("a control cursor must not expose a selected session")
	}
}

func TestShowMoreExpandsOneBlockAndSelectsFirstNewSession(t *testing.T) {
	s := NewState()
	rows := foldingRows(35, "/work/repo", false)
	s.SetRows(rows)
	group := groupID{directory: directoryKey("/work/repo")}

	focusControl(t, &s, listItemShowMore, group)
	s.Handle(KeyEvent{Key: KeyEnter})
	requireSelectedSession(t, s, rows[10].Key)
	if got := countListItems(s.selectableList(), listItemSession); got != 20 {
		t.Fatalf("visible sessions after Enter=%d, want 20", got)
	}

	focusControl(t, &s, listItemShowMore, group)
	s.Handle(KeyEvent{Key: KeyRight})
	requireSelectedSession(t, s, rows[20].Key)
	if got := countListItems(s.selectableList(), listItemSession); got != 30 {
		t.Fatalf("visible sessions after Right=%d, want 30", got)
	}

	focusControl(t, &s, listItemShowMore, group)
	s.Handle(KeyEvent{Key: KeyEnter})
	requireSelectedSession(t, s, rows[30].Key)
	model := s.selectableList()
	if got := countListItems(model, listItemSession); got != 35 {
		t.Fatalf("visible sessions after final partial block=%d, want 35", got)
	}
	if got := countListItems(model, listItemShowMore); got != 0 {
		t.Fatalf("final partial block left %d Show more rows", got)
	}
}

func TestLeftFoldsSelectedDirectoryBlock(t *testing.T) {
	tests := []struct {
		name        string
		selected    int
		wantVisible int
		wantControl listItemKind
	}{
		{name: "first block folds group", selected: 0, wantVisible: 0, wantControl: listItemShowSessions},
		{name: "second block folds to ten", selected: 10, wantVisible: 10, wantControl: listItemShowMore},
		{name: "third block folds to twenty", selected: 20, wantVisible: 20, wantControl: listItemShowMore},
		{name: "fourth block folds to thirty", selected: 30, wantVisible: 30, wantControl: listItemShowMore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewState()
			s.SetRows(foldingRows(45, "/work/repo", false))
			s.selectIndex(tt.selected)
			s.Handle(KeyEvent{Key: KeyLeft})
			group := groupID{directory: directoryKey("/work/repo")}
			requireControlCursor(t, s, tt.wantControl, group)
			if got := countListItems(s.selectableList(), listItemSession); got != tt.wantVisible {
				t.Fatalf("visible sessions=%d, want %d", got, tt.wantVisible)
			}
		})
	}
}

func TestFoldArrowUnspecifiedCasesAreNoOps(t *testing.T) {
	s := NewState()
	s.SetRows(foldingRows(11, "/work/repo", false))
	group := groupID{directory: directoryKey("/work/repo")}
	focusControl(t, &s, listItemShowMore, group)
	before := s.cursor
	s.Handle(KeyEvent{Key: KeyLeft})
	if s.cursor != before {
		t.Fatalf("Left on Show more moved cursor: %+v", s.cursor)
	}

	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyLeft})
	requireControlCursor(t, s, listItemShowSessions, group)
	before = s.cursor
	s.Handle(KeyEvent{Key: KeyLeft})
	if s.cursor != before {
		t.Fatalf("Left on Show sessions moved cursor: %+v", s.cursor)
	}

	s.Handle(KeyEvent{Key: KeyRight})
	requireSelectedSession(t, s, foldingRows(1, "/work/repo", false)[0].Key)
	before = s.cursor
	s.Handle(KeyEvent{Key: KeyRight})
	if s.cursor != before {
		t.Fatalf("Right on session moved cursor: %+v", s.cursor)
	}
}

func TestPinnedFoldsAndRestoresAllSessions(t *testing.T) {
	for _, expandKey := range []Key{KeyEnter, KeyRight} {
		t.Run(expandKeyName(expandKey), func(t *testing.T) {
			s := NewState()
			rows := foldingRows(21, "/work/repo", true)
			s.SetRows(rows)
			s.selectIndex(10)
			s.Handle(KeyEvent{Key: KeyLeft})
			pinned := groupID{pinned: true}
			requireControlCursor(t, s, listItemShowSessions, pinned)
			if got := countListItems(s.selectableList(), listItemSession); got != 0 {
				t.Fatalf("folded Pinned has %d sessions", got)
			}
			s.Handle(KeyEvent{Key: expandKey})
			requireSelectedSession(t, s, rows[0].Key)
			model := s.selectableList()
			if got := countListItems(model, listItemSession); got != 21 {
				t.Fatalf("expanded Pinned has %d sessions, want 21", got)
			}
			if got := countListItems(model, listItemShowMore); got != 0 {
				t.Fatal("Pinned rendered Show more")
			}
		})
	}
}

func expandKeyName(key Key) string {
	if key == KeyEnter {
		return "Enter"
	}
	return "Right"
}

func TestUpDownIncludesControlsInRenderedOrderWithoutWrapping(t *testing.T) {
	rows := append(foldingRows(11, "/work/repo-a", false), foldingRows(1, "/work/repo-b", false)...)
	s := NewState()
	s.SetRows(rows)
	s.selectIndex(9)
	s.Handle(KeyEvent{Key: KeyDown})
	requireControlCursor(t, s, listItemShowMore, groupID{directory: "/work/repo-a"})
	s.Handle(KeyEvent{Key: KeyDown})
	requireSelectedSession(t, s, rows[11].Key)
	s.Handle(KeyEvent{Key: KeyDown})
	requireSelectedSession(t, s, rows[11].Key)
	s.Handle(KeyEvent{Key: KeyUp})
	requireControlCursor(t, s, listItemShowMore, groupID{directory: "/work/repo-a"})
}

func TestBraceNavigationMovesBetweenExpandedAndFoldedGroups(t *testing.T) {
	pinned := foldingRows(2, "/work/pinned", true)
	repoA := foldingRows(11, "/work/repo-a", false)
	repoB := foldingRows(1, "/work/repo-b", false)
	rows := append(append(pinned, repoA...), repoB...)
	s := NewState()
	s.SetRows(rows)

	s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
	requireSelectedSession(t, s, repoA[0].Key)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
	requireSelectedSession(t, s, repoB[0].Key)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
	requireSelectedSession(t, s, repoB[0].Key)

	s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
	requireSelectedSession(t, s, repoA[9].Key)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
	requireSelectedSession(t, s, pinned[1].Key)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
	requireSelectedSession(t, s, pinned[1].Key)

	s.selectIndex(2)
	s.Handle(KeyEvent{Key: KeyLeft})
	requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo-a"})
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
	requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo-a"})
	s.selectIndex(len(rows) - 1)
	s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
	requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo-a"})
}

func TestConfirmationAllowsIssue48NavigationThatMovesCursor(t *testing.T) {
	t.Run("next group", func(t *testing.T) {
		rows := []session.Session{
			{Key: key("a"), CWD: "/work/repo-a", Actions: session.Actions{session.ActionArchive: {Available: true}}},
			{Key: key("b"), CWD: "/work/repo-b"},
		}
		s := NewState()
		s.SetRows(rows)
		s.Handle(KeyEvent{Key: KeyCtrlX})
		s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
		if s.Confirmation != nil {
			t.Fatal("successful group navigation must cancel confirmation")
		}
		requireSelectedSession(t, s, key("b"))
	})

	t.Run("previous group", func(t *testing.T) {
		rows := []session.Session{
			{Key: key("a"), CWD: "/work/repo-a"},
			{Key: key("b"), CWD: "/work/repo-b", Actions: session.Actions{session.ActionArchive: {Available: true}}},
		}
		s := NewState()
		s.SetRows(rows)
		s.selectIndex(1)
		s.Handle(KeyEvent{Key: KeyCtrlX})
		s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
		if s.Confirmation != nil {
			t.Fatal("successful group navigation must cancel confirmation")
		}
		requireSelectedSession(t, s, key("a"))
	})

	t.Run("fold group", func(t *testing.T) {
		rows := []session.Session{{
			Key: key("a"), CWD: "/work/repo", Actions: session.Actions{session.ActionArchive: {Available: true}},
		}}
		s := NewState()
		s.SetRows(rows)
		s.Handle(KeyEvent{Key: KeyCtrlX})
		s.Handle(KeyEvent{Key: KeyLeft})
		if s.Confirmation != nil {
			t.Fatal("successful fold must cancel confirmation")
		}
		requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo"})
	})

	t.Run("expand control", func(t *testing.T) {
		rows := foldingRows(11, "/work/repo", false)
		rows[0].Actions = session.Actions{session.ActionArchive: {Available: true}}
		s := NewState()
		s.SetRows(rows)
		s.Handle(KeyEvent{Key: KeyCtrlX})
		focusControl(t, &s, listItemShowMore, groupID{directory: "/work/repo"})
		s.Handle(KeyEvent{Key: KeyRight})
		if s.Confirmation != nil {
			t.Fatal("successful expansion must cancel confirmation")
		}
		requireSelectedSession(t, s, rows[10].Key)
	})
}

func TestConfirmationSurvivesIssue48NavigationNoOp(t *testing.T) {
	for _, ev := range []KeyEvent{
		{Key: KeyLeft},
		{Key: KeyRight},
		{Key: KeyRune, Rune: '{'},
		{Key: KeyRune, Rune: '}'},
	} {
		t.Run(fmt.Sprintf("key_%d_rune_%q", ev.Key, ev.Rune), func(t *testing.T) {
			s := NewState()
			s.SetRows([]session.Session{{
				Key: key("a"), CWD: "/work/repo", Actions: session.Actions{session.ActionArchive: {Available: true}},
			}})
			s.Handle(KeyEvent{Key: KeyCtrlX})
			if ev.Key == KeyLeft {
				group := groupID{directory: "/work/repo"}
				s.setGroupState(group, groupDisplayState{folded: true})
				focusControl(t, &s, listItemShowSessions, group)
			}
			before := s.cursor
			s.Handle(ev)
			if s.Confirmation == nil {
				t.Fatal("no-op navigation must preserve confirmation")
			}
			if s.cursor != before {
				t.Fatalf("no-op navigation moved cursor from %+v to %+v", before, s.cursor)
			}
		})
	}
}

func TestNonEmptyComposerKeepsEditingAndDispatchPriority(t *testing.T) {
	s := NewState()
	s.SetRows(foldingRows(11, "/work/repo", false))
	s.selectIndex(0)
	s.Composer.Prompt = "ab"
	s.Composer.Cursor = 1
	s.Handle(KeyEvent{Key: KeyLeft})
	if s.Composer.Cursor != 0 {
		t.Fatalf("Left cursor=%d, want 0", s.Composer.Cursor)
	}
	s.Handle(KeyEvent{Key: KeyRight})
	if s.Composer.Cursor != 1 {
		t.Fatalf("Right cursor=%d, want 1", s.Composer.Cursor)
	}
	s.Handle(KeyEvent{Key: KeyRune, Rune: '{'})
	s.Handle(KeyEvent{Key: KeyRune, Rune: '}'})
	if s.Composer.Prompt != "a{}b" {
		t.Fatalf("prompt=%q, want braces inserted", s.Composer.Prompt)
	}
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentDispatch || intent.Prompt != "a{}b" {
		t.Fatalf("Enter intent=%+v, want dispatch", intent)
	}
	if got := countListItems(s.selectableList(), listItemSession); got != 10 {
		t.Fatalf("non-empty composer changed folding: visible=%d", got)
	}
}

func TestWhitespaceOnlyEnterUsesEmptyPromptBehavior(t *testing.T) {
	for _, prompt := range []string{"   ", "\n", " \n "} {
		t.Run(fmt.Sprintf("prompt_%q", prompt), func(t *testing.T) {
			s := NewState()
			s.SetRows([]session.Session{{
				Key: key("a"), CWD: "/work/repo", Actions: session.Actions{session.ActionOpen: {Available: true}},
			}})
			s.Composer.Prompt = prompt
			intent := s.Handle(KeyEvent{Key: KeyEnter})
			if intent.Kind != IntentOpen || intent.Key != key("a") {
				t.Fatalf("intent=%+v, want empty-prompt Open behavior", intent)
			}
		})
	}
}

func TestWhitespaceOnlyEnterStillExpandsSelectedControl(t *testing.T) {
	rows := foldingRows(11, "/work/repo", false)
	s := NewState()
	s.SetRows(rows)
	focusControl(t, &s, listItemShowMore, groupID{directory: "/work/repo"})
	s.Composer.Prompt = "   "
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind == IntentDispatch {
		t.Fatalf("whitespace-only prompt dispatched: %+v", intent)
	}
	requireSelectedSession(t, s, rows[10].Key)
}

func TestComposerCWDForControlRows(t *testing.T) {
	s := NewState()
	s.StartupCWD = "/start"
	rows := append(foldingRows(1, "/work/pinned", true), foldingRows(11, "/work/repo", false)...)
	s.SetRows(rows)

	focusControl(t, &s, listItemShowMore, groupID{directory: "/work/repo"})
	if got := s.ComposerCWD(); got != "/work/repo" {
		t.Fatalf("Show more ComposerCWD=%q", got)
	}
	s.selectIndex(1)
	s.Handle(KeyEvent{Key: KeyLeft})
	if got := s.ComposerCWD(); got != "/work/repo" {
		t.Fatalf("directory Show sessions ComposerCWD=%q", got)
	}
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyLeft})
	if got := s.ComposerCWD(); got != "/start" {
		t.Fatalf("Pinned Show sessions ComposerCWD=%q, want StartupCWD", got)
	}
}

func TestRefreshKeepsSelectedSessionVisibleAcrossPageBoundary(t *testing.T) {
	rows := foldingRows(10, "/work/repo", false)
	s := NewState()
	s.SetRows(rows)
	s.selectIndex(9)
	selected := rows[9].Key
	newest := session.Session{Key: key("newest"), Name: "newest", CWD: "/work/repo", CreatedAt: time.Unix(100, 0)}
	s.SetRows(append([]session.Session{newest}, rows...))
	requireSelectedSession(t, s, selected)
	if got := countListItems(s.selectableList(), listItemSession); got != 11 {
		t.Fatalf("visible sessions=%d, want selected boundary-crossing row visible", got)
	}
}

func TestRefreshPreservesOrReconcilesControlCursor(t *testing.T) {
	repoA := foldingRows(11, "/work/repo-a", false)
	repoB := foldingRows(1, "/work/repo-b", false)
	s := NewState()
	s.SetRows(append(repoA, repoB...))
	groupA := groupID{directory: "/work/repo-a"}
	focusControl(t, &s, listItemShowMore, groupA)
	s.SetRows(append(append([]session.Session(nil), repoA...), repoB...))
	requireControlCursor(t, s, listItemShowMore, groupA)

	s.SetRows(append(repoA[:10], repoB...))
	requireSelectedSession(t, s, repoB[0].Key)
	s.SetRows(nil)
	if s.hasCursor {
		t.Fatalf("empty catalog retained cursor %+v", s.cursor)
	}
}

func TestDisappearingControlFallsBackToPreviousWhenNoNextSurvives(t *testing.T) {
	repoA := foldingRows(1, "/work/repo-a", false)
	repoB := foldingRows(11, "/work/repo-b", false)
	s := NewState()
	s.SetRows(append(repoA, repoB...))
	focusControl(t, &s, listItemShowMore, groupID{directory: "/work/repo-b"})
	s.SetRows(append(repoA, repoB[:10]...))
	requireSelectedSession(t, s, repoB[9].Key)
}

func TestFoldStateSurvivesScopeAbsenceAndHeadingChange(t *testing.T) {
	repoA := foldingRows(11, "/work/repo-a", false)
	repoB := foldingRows(1, "/work/repo-b", false)
	s := NewState()
	s.SetRows(repoA)
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyLeft})
	requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo-a"})

	s.SetRows(repoB)
	s.SetRows(append(repoA, repoB...))
	_, _, ok := s.selectableList().groupForItem(controlItemID(groupID{directory: "/work/repo-a"}, listItemShowSessions))
	if !ok {
		t.Fatal("repo-a fold state did not survive disappearance and Recently created/path heading change")
	}
}

func TestUpDownVisitsEverySelectableItemInModelOrder(t *testing.T) {
	rows := append(foldingRows(2, "/work/pinned", true), foldingRows(11, "/work/repo-a", false)...)
	rows = append(rows, foldingRows(1, "/work/repo-b", false)...)
	s := NewState()
	s.SetRows(rows)
	model := s.selectableList()
	for i, want := range model.items {
		if !s.hasCursor || s.cursor != want.id {
			t.Fatalf("position %d cursor=%+v, want %+v", i, s.cursor, want.id)
		}
		view := visibleText(s.View(100, 40))
		label := "Show more"
		if want.id.kind == listItemSession {
			label = s.Rows[want.rowIndex].DisplayName()
		} else if want.id.kind == listItemShowSessions {
			label = "Show sessions"
		}
		selectedLineMatches := false
		for _, line := range strings.Split(view, "\n") {
			if strings.HasPrefix(line, "> ") && strings.Contains(line, label) {
				selectedLineMatches = true
				break
			}
		}
		if !selectedLineMatches {
			t.Fatalf("position %d is not rendered as selected %q:\n%s", i, label, view)
		}
		if i+1 < len(model.items) {
			s.Handle(KeyEvent{Key: KeyDown})
		}
	}
}

func TestControlCursorCannotTargetSessionActions(t *testing.T) {
	s := NewState()
	rows := foldingRows(11, "/work/repo", false)
	rows[0].Actions = session.Actions{
		session.ActionOpen:   {Available: true},
		session.ActionStop:   {Available: true},
		session.ActionRename: {Available: true},
	}
	s.SetRows(rows)
	focusControl(t, &s, listItemShowMore, groupID{directory: "/work/repo"})
	for _, key := range []Key{KeyCtrlO, KeyCtrlX, KeyCtrlR, KeyCtrlT} {
		s.Error = ""
		intent := s.Handle(KeyEvent{Key: key})
		if intent.Kind != IntentNone || s.Error != "No session selected" {
			t.Fatalf("key=%v intent=%+v error=%q", key, intent, s.Error)
		}
	}
}

func TestPinExpandsFoldedPinnedToKeepSelectedSessionVisible(t *testing.T) {
	rows := append(foldingRows(1, "/work/pinned", true), foldingRows(1, "/work/repo", false)...)
	s := NewState()
	s.SetRows(rows)
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyLeft})
	s.selectIndex(1)
	pinned := true
	s.ApplyPatch(sessionctl.Patch{Key: rows[1].Key, Pinned: &pinned})
	requireSelectedSession(t, s, rows[1].Key)
	if got := countListItems(s.selectableList(), listItemSession); got != 2 {
		t.Fatalf("Pinned stayed folded after pinning selected session: visible=%d", got)
	}
}

func TestUnpinSelectedPinnedCanChooseFoldedFollowingGroupControl(t *testing.T) {
	rows := append(foldingRows(1, "/work/pinned", true), foldingRows(1, "/work/repo", false)...)
	s := NewState()
	s.SetRows(rows)
	s.selectIndex(1)
	s.Handle(KeyEvent{Key: KeyLeft})
	s.selectIndex(0)
	unpinned := false
	s.ApplyPatch(sessionctl.Patch{Key: rows[0].Key, Pinned: &unpinned})
	requireControlCursor(t, s, listItemShowSessions, groupID{directory: "/work/repo"})
}
