package session

import "sort"

// SortOverview sorts sessions into the canonical Agent View order: pinned
// sessions first, each group ordered by CreatedAt descending, ties broken
// by session key. This is the single ordering rule shared by a provider
// reload and any in-memory update (e.g. a local pin toggle) that must
// reproduce the same order without one -- see the DesignDoc's "session は
// 作成時刻が新しい順に並べる...Activity や runtime status の変化だけでは
// 並び順を変更しない" (Activity/Runtime are deliberately not sort keys).
func SortOverview(sessions []Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.Key.String() < b.Key.String()
	})
}
