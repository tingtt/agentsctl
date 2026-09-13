package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// This file implements the Project-scoped cursor pagination spike (README "ChatGPT Project
// session listing: cursor pagination spike"): GET
// /backend-api/gizmos/{project_id}/conversations?cursor={cursor}, tested as a replacement for the
// offset/hide_snorlax-based global-endpoint path documented earlier in this README (Phase 5 and
// its addendum). Schema field names below come from a real captured response, not guesswork: the
// collection field is `items`, the per-item identity is `id`, and the two timestamps are
// `create_time`/`update_time`. The response's own `cursor` field is opaque: this code only ever
// compares it for exact equality against previously-used request cursors (cycle detection) and
// passes it back to the next request verbatim — never parsed, incremented, or compared
// numerically, per the task's explicit prohibition.

// cursorConversation is one Project conversation observed via the cursor-paginated endpoint.
// Deliberately narrower than the existing `conversation` type (no Title, Discriminator, or
// AvailableKeys): this spike's scope is List completeness only (README "Important" section), and
// nothing here needs a conversation's title or content to prove or disprove that. Only ID and the
// two timestamps required for local `session created_at DESC` ordering (README "Ordering is
// local") cross into this type.
type cursorConversation struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// cursorConversationWireItem is the bridge's per-item wire shape for one cursor-page conversation.
// Timestamps stay strings across the wire (ISO 8601, e.g. "2026-09-04T19:38:23.939008Z" in the
// captured example) and are parsed into cursorConversation.CreatedAt/UpdatedAt on the Go side.
type cursorConversationWireItem struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// cursorPageWireResult is the bridge's response to one projectConversationsCursor call.
// HasNextCursor/NextCursor are computed bridge-side from whatever shape the response's `cursor`
// field actually has (absent key, null, or a non-empty string all currently sanitize to
// HasNextCursor=false — see bridge/preload.js's projectConversationsCursorFrom; a live run
// records which shape was actually observed as the terminal representation, per README Phase A).
type cursorPageWireResult struct {
	Items         []cursorConversationWireItem `json:"items"`
	RawItemCount  int                          `json:"rawItemCount"`
	HasNextCursor bool                         `json:"hasNextCursor"`
	NextCursor    string                       `json:"nextCursor"`
}

// cursorFetchedPage pairs the opaque cursor value used to REQUEST one page (CursorIn — "0" for
// the documented entry point, README "Cursor is opaque") with what that page's response said
// about the next one. This is the unit accumulateCursorChain walks.
type cursorFetchedPage struct {
	CursorIn      string
	Conversations []cursorConversation
	HasNextCursor bool
	NextCursor    string
}

// redactedCursor is the safe-to-print form of an opaque cursor value for diagnostics and error
// messages (README security boundary: "raw cursor を不要にログしない"). "0" is the documented,
// non-sensitive entry point and is shown verbatim; every other value is fingerprinted.
func redactedCursor(cursor string) string {
	if cursor == "" {
		return "<empty>"
	}
	if cursor == "0" {
		return "0"
	}
	sum := sha256.Sum256([]byte(cursor))
	return "cursor:" + hex.EncodeToString(sum[:])[:12]
}

// accumulateCursorChain is the pure, unit-tested core of Project cursor-pagination completeness.
// It walks `pages` in the order they were actually fetched, requiring:
//
//   - a contiguous chain: page[i].CursorIn must equal page[i-1].NextCursor (page[0].CursorIn is
//     whatever the caller started from, normally "0");
//   - no cursor cycle: a page's NextCursor must never equal a CursorIn value already used earlier
//     in the walk — this is a FAIL CLOSED condition (README "Safety against infinite loops"), not
//     merely "incomplete", and is detected as soon as the repeated value is returned, without
//     needing to actually re-fetch it;
//   - every conversation carries a non-empty, stable ID (a missing ID fails closed); and
//   - no more than maxPages pages (the same defensive bound used by mergeProjectPages elsewhere in
//     this spike).
//
// Completeness requires the LAST page in the sequence to report HasNextCursor == false (a
// terminal cursor explicitly observed), with no error anywhere in the walk. Deduplication is by
// conversation ID; duplicatesObserved counts (page, item) occurrences superseded by an earlier
// occurrence of the same ID — expected at a page boundary (README Phase G) and never itself an
// error.
func accumulateCursorChain(pages []cursorFetchedPage, maxPages int) (conversations []cursorConversation, duplicatesObserved int, complete bool, err error) {
	if len(pages) == 0 {
		return nil, 0, false, fmt.Errorf("no pages observed")
	}
	if len(pages) > maxPages {
		return nil, 0, false, fmt.Errorf("excessive page count: got %d pages, max %d", len(pages), maxPages)
	}
	seenCursorIn := make(map[string]bool, len(pages))
	byID := make(map[string]cursorConversation)
	expected := pages[0].CursorIn
	for i, p := range pages {
		if p.CursorIn != expected {
			return nil, 0, false, fmt.Errorf("page %d: cursor chain broken — requested with %s, expected %s",
				i, redactedCursor(p.CursorIn), redactedCursor(expected))
		}
		if seenCursorIn[p.CursorIn] {
			return nil, 0, false, fmt.Errorf("page %d: cursor cycle detected — %s was already used as a request cursor",
				i, redactedCursor(p.CursorIn))
		}
		seenCursorIn[p.CursorIn] = true
		for _, item := range p.Conversations {
			if item.ID == "" {
				return nil, 0, false, fmt.Errorf("page %d: conversation missing stable identity", i)
			}
			if _, dup := byID[item.ID]; dup {
				duplicatesObserved++
			}
			byID[item.ID] = item
		}
		if p.HasNextCursor {
			if p.NextCursor == "" {
				return nil, 0, false, fmt.Errorf("page %d: response declared a next cursor but the value is empty", i)
			}
			if seenCursorIn[p.NextCursor] {
				return nil, 0, false, fmt.Errorf("page %d: cursor cycle detected — next cursor %s was already used as a request cursor",
					i, redactedCursor(p.NextCursor))
			}
			expected = p.NextCursor
		} else if i != len(pages)-1 {
			return nil, 0, false, fmt.Errorf("page %d: reported terminal but is not the last page fetched", i)
		}
	}
	conversations = make([]cursorConversation, 0, len(byID))
	for _, c := range byID {
		conversations = append(conversations, c)
	}
	last := pages[len(pages)-1]
	return conversations, duplicatesObserved, !last.HasNextCursor, nil
}

// sortConversationsByCreatedDesc returns a NEW slice ordered by CreatedAt descending (newest
// first) — agentsctl's local display order (README "Ordering is local"), independent of whatever
// order the server response used. Ties (identical CreatedAt) break on ID ascending, so the result
// is fully deterministic regardless of input order — cursorConversation has no pin/star field at
// all, so this function structurally cannot let remote pin state influence ordering (README
// "Pin state is local-only").
func sortConversationsByCreatedDesc(conversations []cursorConversation) []cursorConversation {
	result := make([]cursorConversation, len(conversations))
	copy(result, conversations)
	sort.SliceStable(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

// isNonIncreasingByCreated/isNonIncreasingByUpdated are privacy-safe (booleans only, no IDs or
// timestamps) diagnostics for README Phase F: do successive conversations in the given order have
// non-increasing CreatedAt/UpdatedAt? Applied to a page's ORIGINAL server-returned order, these
// answer "does the server appear to order by creation time, update time, or neither" without ever
// printing a real timestamp or ID.
func isNonIncreasingByCreated(conversations []cursorConversation) bool {
	for i := 1; i < len(conversations); i++ {
		if conversations[i].CreatedAt.After(conversations[i-1].CreatedAt) {
			return false
		}
	}
	return true
}

func isNonIncreasingByUpdated(conversations []cursorConversation) bool {
	for i := 1; i < len(conversations); i++ {
		if conversations[i].UpdatedAt.After(conversations[i-1].UpdatedAt) {
			return false
		}
	}
	return true
}

// parseCursorWireItems converts the bridge's per-item wire shape into cursorConversation,
// failing closed (README Phase H) on a missing ID, a missing create_time, or a create_time/
// update_time that cannot be parsed as RFC 3339 — the format observed in the captured example
// (e.g. "2026-09-04T19:38:23.939008Z").
func parseCursorWireItems(items []cursorConversationWireItem) ([]cursorConversation, error) {
	result := make([]cursorConversation, 0, len(items))
	for i, wireItem := range items {
		if wireItem.ID == "" {
			return nil, fmt.Errorf("item %d: missing stable identity", i)
		}
		if wireItem.CreatedAt == "" {
			return nil, fmt.Errorf("item %d: missing create_time", i)
		}
		created, err := time.Parse(time.RFC3339Nano, wireItem.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("item %d: unparseable create_time: %w", i, err)
		}
		var updated time.Time
		if wireItem.UpdatedAt != "" {
			updated, err = time.Parse(time.RFC3339Nano, wireItem.UpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("item %d: unparseable update_time: %w", i, err)
			}
		}
		result = append(result, cursorConversation{ID: wireItem.ID, CreatedAt: created, UpdatedAt: updated})
	}
	return result, nil
}

// fetchCursorPage issues one projectConversationsCursor bridge call. The bridge self-issues the
// authenticated same-origin fetch (the same mechanism already proven for the existing
// no-cursor `conversations` method — see README Phase 1/4/5 — which, unlike the global
// /backend-api/conversations endpoint, does not 401 on a script-issued fetch); no cookie, token,
// or header ever crosses into this Go process.
func fetchCursorPage(client net.Conn, id int, projectID, cursor string) (cursorPageWireResult, error) {
	raw, err := call(client, request{ID: id, Method: "projectConversationsCursor", ProjectID: projectID, Params: map[string]any{"cursor": cursor}})
	if err != nil {
		return cursorPageWireResult{}, err
	}
	var result cursorPageWireResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return cursorPageWireResult{}, fmt.Errorf("decode cursor page: %w", err)
	}
	return result, nil
}

// enumerateProjectConversationsByCursorSelfFetch drives the cursor-chain fetch loop by having the
// bridge self-issue each request (README "Fetch loop"), starting at cursor="0" and continuing
// until a page explicitly reports no next cursor, or maxPages is exceeded.
//
// LIVE EVIDENCE (2026-09-13): a self-issued fetch of this endpoint WITH a `cursor` query
// parameter returns HTTP 401, even though the existing no-cursor `conversations` method's
// self-issued fetch of the exact same path (no query string) succeeds — see the README's
// "self-initiated fetch of cursor-paginated endpoint: NOT AUTHORIZED" evidence. This mirrors the
// already-documented `/backend-api/conversations` 401 (Phase 5 addendum): something beyond
// cookies (most likely a per-request anti-automation token the real ChatGPT client computes) is
// required once a `cursor` parameter is present, and this bridge does not have — and must not try
// to obtain — that. This function is therefore kept ONLY as a documented negative probe (like
// `globalConversationsPage` above), never as the primary enumeration mechanism;
// runCursorExperiment uses enumerateProjectConversationsByCursorPassive instead.
func enumerateProjectConversationsByCursorSelfFetch(client net.Conn, projectID string, maxPages, idBase int) (conversations []cursorConversation, duplicatesObserved int, complete bool, pagesFetched int, firstPageOrder []cursorConversation, err error) {
	var pages []cursorFetchedPage
	cursor := "0"
	for i := 0; i < maxPages; i++ {
		wire, ferr := fetchCursorPage(client, idBase+i, projectID, cursor)
		if ferr != nil {
			return nil, 0, false, len(pages), firstPageOrder, fmt.Errorf("fetch cursor page %d (cursor_in=%s): %w", i, redactedCursor(cursor), ferr)
		}
		items, perr := parseCursorWireItems(wire.Items)
		if perr != nil {
			return nil, 0, false, len(pages), firstPageOrder, fmt.Errorf("page %d (cursor_in=%s): %w", i, redactedCursor(cursor), perr)
		}
		pages = append(pages, cursorFetchedPage{
			CursorIn: cursor, Conversations: items, HasNextCursor: wire.HasNextCursor, NextCursor: wire.NextCursor,
		})
		if i == 0 {
			firstPageOrder = items
		}
		outLabel := "<terminal>"
		if wire.HasNextCursor {
			outLabel = redactedCursor(wire.NextCursor)
		}
		fmt.Printf("page=%d conversation_count=%d cursor_in=%s cursor_out=%s\n", i, len(items), redactedCursor(cursor), outLabel)

		convs, dups, comp, aerr := accumulateCursorChain(pages, maxPages)
		if aerr != nil {
			return nil, 0, false, len(pages), firstPageOrder, fmt.Errorf("page %d: %w", i, aerr)
		}
		if comp {
			return convs, dups, true, len(pages), firstPageOrder, nil
		}
		cursor = wire.NextCursor
	}
	return nil, 0, false, len(pages), firstPageOrder, fmt.Errorf("exceeded max pages (%d) without observing a terminal cursor", maxPages)
}

// cursorCaptureWireItem is one bridge-observed, passively-captured page of the cursor-paginated
// project-scoped endpoint (bridge/main.js's recordProjectConversationsCursorCapture). CaptureID is
// a bridge-internal arrival-order reference, never a pagination position. A request with no
// `cursor` parameter at all is never recorded here (a structurally different request shape).
//
// CursorIn alone is NOT sufficient pagination identity: live evidence (README seventh/eighth live
// run) found this endpoint's own `?cursor=0` explicit entry point is issued by at least TWO
// structurally different real requests (a 5-item one and a 10-item one, both explicitly
// cursor="0"), most likely distinguished by a `limit` or similar parameter this bridge does not
// otherwise track. SeriesKey (canonicalCursorSeriesKeyFrom — everything but `cursor`, digested)
// is the real series identity, mirroring the same SeriesKey concept already used for the global
// endpoint's offset pagination; captures must be grouped by SeriesKey FIRST, then by CursorIn
// within one series, before ever comparing two captures' conversation sets for equality.
type cursorCaptureWireItem struct {
	CaptureID     int                          `json:"captureID"`
	CursorIn      string                       `json:"cursorIn"`
	SeriesKey     string                       `json:"seriesKey"`
	Items         []cursorConversationWireItem `json:"items"`
	RawItemCount  int                          `json:"rawItemCount"`
	HasNextCursor bool                         `json:"hasNextCursor"`
	NextCursor    string                       `json:"nextCursor"`
}

func fetchCursorCaptures(client net.Conn, id int, projectID string) ([]cursorCaptureWireItem, error) {
	raw, err := call(client, request{ID: id, Method: "projectConversationsCursorCaptures", ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	var result []cursorCaptureWireItem
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode cursor captures: %w", err)
	}
	return result, nil
}

// conversationIDSet returns a canonical, sorted, comma-joined form of a page's conversation IDs —
// used only to compare whether two observations of the same page returned the same underlying
// data, never printed or logged (raw IDs stay in-process, same as everywhere else in this bridge
// protocol).
func conversationIDSet(items []cursorConversationWireItem) string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// dedupeCapturesByCursorIn collapses passively-observed captures (which can include a benign
// duplicate — e.g. a React re-render re-issuing the exact same request) down to one page per
// distinct CursorIn value, keeping the LATEST-arriving (highest CaptureID) observation for each.
//
// Live evidence (2026-09-14): real-wheel-triggered traffic observed the SAME CursorIn ("0")
// fetched many times in quick succession (42 cursor-present 200 responses across two short wheel
// bursts), and two of those observations reported a different declared next-cursor token for an
// otherwise-unchanged page. Comparing the raw NextCursor token for equality (the original
// implementation) is therefore too strict: nothing in this endpoint's observed behavior — or in
// the task's own "opaque cursor" discipline, which forbids assuming a token is idempotent —
// guarantees the SAME logical page returns the SAME next-cursor token on every fetch. What matters
// for correctness is whether the returned CONVERSATION SET actually changed, not whether the
// opaque continuation token happened to differ.
//
// Equality is therefore judged by conversationIDSet (the sorted set of conversation IDs a page
// actually returned): two observations of the same CursorIn with the SAME ID set are treated as
// the same page, keeping the freshest (highest CaptureID) one's own HasNextCursor/NextCursor to
// continue the walk from — never an older, possibly-since-invalidated token. Two observations of
// the same CursorIn with a DIFFERENT ID set is genuine schema drift or account activity
// mid-enumeration, and still fails closed exactly as before.
func dedupeCapturesByCursorIn(captures []cursorCaptureWireItem) ([]cursorCaptureWireItem, error) {
	ordered := make([]cursorCaptureWireItem, len(captures))
	copy(ordered, captures)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CaptureID < ordered[j].CaptureID })
	latest := make(map[string]cursorCaptureWireItem, len(ordered))
	firstIDSet := make(map[string]string, len(ordered))
	order := make([]string, 0, len(ordered))
	for _, c := range ordered {
		idSet := conversationIDSet(c.Items)
		prevIDSet, ok := firstIDSet[c.CursorIn]
		if !ok {
			firstIDSet[c.CursorIn] = idSet
			order = append(order, c.CursorIn)
			latest[c.CursorIn] = c
			continue
		}
		if prevIDSet != idSet {
			return nil, fmt.Errorf("cursor %s was observed twice with a different conversation set (schema drift or account activity mid-enumeration)", redactedCursor(c.CursorIn))
		}
		latest[c.CursorIn] = c
	}
	result := make([]cursorCaptureWireItem, 0, len(order))
	for _, cursorIn := range order {
		result = append(result, latest[cursorIn])
	}
	return result, nil
}

// cursorInVariantSummary is a privacy-safe forensic summary of every observation of ONE CursorIn
// value within a single harvest, computed only when dedupeCapturesByCursorIn's fail-closed check
// fires (README fifth/sixth live run: cursor_in="0" observed with a genuinely different
// conversation set, even in a run intended to be free of concurrent Project activity). It exists
// to distinguish, from evidence rather than guessing: flickering between a small, stable number of
// alternating states (e.g. two backend replicas or cache layers disagreeing) versus continuous
// drift (e.g. genuine ongoing account activity) versus a capture-pipeline bug — without ever
// exposing a raw conversation ID or cursor value.
type cursorInVariantSummary struct {
	Observations        int
	DistinctIDSets      int
	DistinctNextCursors int
	MinItemCount        int
	MaxItemCount        int
}

// summarizeCursorInVariants groups captures by CursorIn and computes cursorInVariantSummary for
// each. Pure and unit-tested; the live driver calls this only for diagnostics when a conflict has
// already been detected, never to decide completeness itself.
func summarizeCursorInVariants(captures []cursorCaptureWireItem) map[string]cursorInVariantSummary {
	byCursorIn := make(map[string][]cursorCaptureWireItem, len(captures))
	for _, c := range captures {
		byCursorIn[c.CursorIn] = append(byCursorIn[c.CursorIn], c)
	}
	summaries := make(map[string]cursorInVariantSummary, len(byCursorIn))
	for cursorIn, group := range byCursorIn {
		idSets := make(map[string]bool, len(group))
		nextCursors := make(map[string]bool, len(group))
		minCount, maxCount := -1, -1
		for _, c := range group {
			idSets[conversationIDSet(c.Items)] = true
			nextCursors[c.NextCursor] = true
			if minCount == -1 || c.RawItemCount < minCount {
				minCount = c.RawItemCount
			}
			if c.RawItemCount > maxCount {
				maxCount = c.RawItemCount
			}
		}
		summaries[cursorIn] = cursorInVariantSummary{
			Observations: len(group), DistinctIDSets: len(idSets),
			DistinctNextCursors: len(nextCursors), MinItemCount: minCount, MaxItemCount: maxCount,
		}
	}
	return summaries
}

func printCursorInVariants(captures []cursorCaptureWireItem) {
	summaries := summarizeCursorInVariants(captures)
	cursorIns := make([]string, 0, len(summaries))
	for cursorIn := range summaries {
		cursorIns = append(cursorIns, cursorIn)
	}
	sort.Strings(cursorIns)
	for _, cursorIn := range cursorIns {
		s := summaries[cursorIn]
		fmt.Printf("cursor_in=%s variants: observations=%d distinct_id_sets=%d distinct_next_cursors=%d item_count_range=%d-%d\n",
			redactedCursor(cursorIn), s.Observations, s.DistinctIDSets, s.DistinctNextCursors, s.MinItemCount, s.MaxItemCount)
	}
}

// filterCapturesNewerThan returns only the captures strictly newer than minCaptureIDExclusive —
// the pure filtering core of enumerateProjectConversationsByCursorPassive's freshness watermark
// (see that function's doc comment for the live bug this fixes). Mirrors the same pattern already
// proven for the pin experiment's selectFreshMatchDiagnostics: keeping this as a small, separately
// testable pure function is what makes the watermark logic unit-testable at all, since the
// surrounding function itself needs a live bridge connection.
func filterCapturesNewerThan(captures []cursorCaptureWireItem, minCaptureIDExclusive int) []cursorCaptureWireItem {
	var fresh []cursorCaptureWireItem
	for _, c := range captures {
		if c.CaptureID > minCaptureIDExclusive {
			fresh = append(fresh, c)
		}
	}
	return fresh
}

// groupCapturesBySeriesKey partitions captures by SeriesKey (README seventh/eighth live run: two
// structurally different real requests were found sharing the same CursorIn but different other
// query parameters). Pure and unit-tested.
func groupCapturesBySeriesKey(captures []cursorCaptureWireItem) map[string][]cursorCaptureWireItem {
	groups := make(map[string][]cursorCaptureWireItem)
	for _, c := range captures {
		groups[c.SeriesKey] = append(groups[c.SeriesKey], c)
	}
	return groups
}

// selectSeriesMatchingObservedLinkCount picks, among possibly-multiple distinct real request
// series sharing the same endpoint, the one whose EARLIEST (lowest CaptureID) capture's item count
// exactly matches expectedFirstPageSize — an independent signal (the Project scroll region's own
// DOM-observed conversation-link count, read once via fetchProjectScrollRegion before any
// pagination begins), not derived from network captures at all. This deliberately fails closed
// (ok=false) rather than guess when zero or more than one series matches: silently picking "the
// biggest" or "the most common" series risks walking a decoy/secondary UI widget's own cursor
// chain to a false COMPLETE while under-counting the real Project conversation list — a wrong
// answer that looks like a right one, which is worse than reporting NOT VERIFIED.
func selectSeriesMatchingObservedLinkCount(groups map[string][]cursorCaptureWireItem, expectedFirstPageSize int) (seriesKey string, ok bool) {
	var matched []string
	for key, group := range groups {
		earliest := group[0]
		for _, c := range group {
			if c.CaptureID < earliest.CaptureID {
				earliest = c
			}
		}
		if len(earliest.Items) == expectedFirstPageSize {
			matched = append(matched, key)
		}
	}
	if len(matched) != 1 {
		return "", false
	}
	return matched[0], true
}

// filterCapturesBySeriesKey keeps only captures belonging to one selected series — the pure
// counterpart to filterCapturesNewerThan, applied after series selection so a pinned series is
// never contaminated by another real request shape sharing the same endpoint.
func filterCapturesBySeriesKey(captures []cursorCaptureWireItem, seriesKey string) []cursorCaptureWireItem {
	var selected []cursorCaptureWireItem
	for _, c := range captures {
		if c.SeriesKey == seriesKey {
			selected = append(selected, c)
		}
	}
	return selected
}

func printSeriesGroups(groups map[string][]cursorCaptureWireItem) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		earliest := group[0]
		for _, c := range group {
			if c.CaptureID < earliest.CaptureID {
				earliest = c
			}
		}
		fmt.Printf("series=%s observations=%d first_page_item_count=%d\n", shortDigest(key), len(group), len(earliest.Items))
	}
}

// enumerateProjectConversationsByCursorPassive is the primary enumeration mechanism (see the
// self-fetch 401 evidence on enumerateProjectConversationsByCursorSelfFetch above): it navigates
// into the Project's own view (which is what naturally issues this endpoint's first real request —
// live evidence 2026-09-13 confirmed scrolling that view does load further cursor pages), then
// repeatedly harvests whatever the real ChatGPT client has passively been observed requesting,
// running accumulateCursorChain after every harvest — exactly the same pure, unit-tested logic
// exercised directly by cursor_test.go — BEFORE sending another real wheel input. If no terminal
// cursor is reached within maxWheelAttempts, this returns an error (never a partial result
// presented as complete), mirroring every other fail-closed enumeration in this spike.
//
// A second live run (2026-09-13) hit exactly the same class of bug this spike's pin experiment
// already found and fixed once for the global endpoint (README "sixth pass — added an explicit
// freshness gate"): earlier code considered EVERY capture ever observed for this Project,
// including one from this endpoint's very first, unrelated navigation near the top of run() (via
// the existing `conversations` bridge method's own cache-miss navigation) — long before this
// function's own `navigateProject` call. Fixed with the same watermark pattern already proven
// elsewhere in this spike: a baseline max CaptureID is read BEFORE this function's own navigation,
// and only captures strictly newer than that watermark are ever considered.
//
// A third live run confirmed the trigger mechanism itself needed to change: synthetic DOM scroll
// (`container.scrollTop = ...` plus a dispatched `scroll` Event, via simulateSidebarScroll) does
// not reliably reproduce whatever browser-level input-pipeline behavior the real ChatGPT
// frontend's own pagination trigger depends on — the operator's manual, real mouse-wheel input
// was directly observed to work. This now sends real Electron `sendInputEvent` mouseWheel input
// (bridge/main.js's realWheelScrollProject) at the Project's own conversation-list viewport
// coordinate, instead of any synthetic DOM event, and never a self-issued fetch of the cursor
// endpoint (README Task 7).
//
// A seventh/eighth live run found CursorIn alone is not sufficient pagination identity: this
// endpoint's own `?cursor=0` explicit entry point was issued by at least TWO structurally
// different real requests (a 5-item one and a 10-item one), which dedupeCapturesByCursorIn
// correctly (but for the wrong underlying reason) flagged as "the same page reporting a different
// conversation set" every time both happened to appear in the same run — explaining why this
// failed identically and reproducibly across three separate live runs regardless of timing. Fixed
// by grouping fresh captures by SeriesKey (canonicalCursorSeriesKeyFrom, mirroring the same concept
// already used for the global endpoint's offset pagination) before ever comparing two captures'
// conversation sets, and selecting the one series whose first page's item count matches the
// Project scroll region's own independently-observed DOM link count — never guessing when that
// match is ambiguous (selectSeriesMatchingObservedLinkCount fails closed rather than picking "the
// biggest" or "the most common" series, which risks walking a decoy widget's own chain to a false
// COMPLETE while under-counting the real list). The selected series is pinned for the rest of the
// walk once chosen, so a later capture of the excluded series can never contaminate it.
func enumerateProjectConversationsByCursorPassive(client net.Conn, projectID string, maxWheelAttempts, maxPages, idBase int) (conversations []cursorConversation, duplicatesObserved int, complete bool, pagesFetched int, firstPageOrder []cursorConversation, err error) {
	baseline, err := fetchCursorCaptures(client, idBase, projectID)
	if err != nil {
		return nil, 0, false, 0, nil, fmt.Errorf("read baseline cursor captures: %w", err)
	}
	watermark := 0
	for _, c := range baseline {
		if c.CaptureID > watermark {
			watermark = c.CaptureID
		}
	}

	if _, err := call(client, request{ID: idBase + 1, Method: "navigateProject", ProjectID: projectID}); err != nil {
		return nil, 0, false, 0, nil, fmt.Errorf("navigate to Project view: %w", err)
	}
	fmt.Println("navigate Project: PASS")
	time.Sleep(2 * time.Second)

	region, rerr := fetchProjectScrollRegion(client, idBase+2)
	if rerr != nil {
		return nil, 0, false, 0, nil, fmt.Errorf("read initial Project scroll region: %w", rerr)
	}
	printProjectScrollRegion("target region", region)

	var pinnedSeriesKey string
	var seriesPinned bool

	harvest := func(id int) (pages []cursorFetchedPage, firstOrder []cursorConversation, err error) {
		raw, err := fetchCursorCaptures(client, id, projectID)
		if err != nil {
			return nil, nil, err
		}
		fresh := filterCapturesNewerThan(raw, watermark)

		if !seriesPinned && len(fresh) > 0 {
			groups := groupCapturesBySeriesKey(fresh)
			if len(groups) == 1 {
				for key := range groups {
					pinnedSeriesKey = key
				}
				seriesPinned = true
			} else {
				key, ok := selectSeriesMatchingObservedLinkCount(groups, region.ProjectConversationLinks)
				if !ok {
					printSeriesGroups(groups)
					return nil, nil, fmt.Errorf("%d distinct real request series observed for this endpoint and none uniquely matched the %d-link Project view observed in the DOM (see series diagnostics above) — refusing to guess which is the real Project conversation list", len(groups), region.ProjectConversationLinks)
				}
				pinnedSeriesKey = key
				seriesPinned = true
				fmt.Printf("selected series: %s (matches the %d-link Project view observed in the DOM; %d other series excluded)\n", shortDigest(pinnedSeriesKey), region.ProjectConversationLinks, len(groups)-1)
			}
		}
		if seriesPinned {
			fresh = filterCapturesBySeriesKey(fresh, pinnedSeriesKey)
		}

		deduped, err := dedupeCapturesByCursorIn(fresh)
		if err != nil {
			printCursorInVariants(fresh)
			return nil, nil, err
		}
		for i, c := range deduped {
			items, perr := parseCursorWireItems(c.Items)
			if perr != nil {
				return nil, nil, fmt.Errorf("capture cursor_in=%s: %w", redactedCursor(c.CursorIn), perr)
			}
			if i == 0 {
				firstOrder = items
			}
			pages = append(pages, cursorFetchedPage{
				CursorIn: c.CursorIn, Conversations: items, HasNextCursor: c.HasNextCursor, NextCursor: c.NextCursor,
			})
		}
		return pages, firstOrder, nil
	}

	tryAccumulate := func(id int) (done bool, retConvs []cursorConversation, retDups int, retErr error) {
		pages, firstOrder, herr := harvest(id)
		if herr != nil {
			return true, nil, 0, herr
		}
		if len(pages) == 0 {
			return false, nil, 0, nil
		}
		for i, p := range pages {
			outLabel := "<terminal>"
			if p.HasNextCursor {
				outLabel = redactedCursor(p.NextCursor)
			}
			fmt.Printf("page=%d conversation_count=%d cursor_in=%s cursor_out=%s\n", i, len(p.Conversations), redactedCursor(p.CursorIn), outLabel)
		}
		convs, dups, comp, aerr := accumulateCursorChain(pages, maxPages)
		if aerr != nil {
			return true, nil, 0, aerr
		}
		pagesFetched, firstPageOrder = len(pages), firstOrder
		if comp {
			return true, convs, dups, nil
		}
		return false, nil, 0, nil
	}

	if done, convs, dups, herr := tryAccumulate(idBase + 3); done {
		if herr != nil {
			return nil, 0, false, pagesFetched, firstPageOrder, herr
		}
		return convs, dups, true, pagesFetched, firstPageOrder, nil
	}

	for attempt := 0; attempt < maxWheelAttempts; attempt++ {
		wheelResult, werr := fetchRealWheelScrollProject(client, idBase+4+attempt*2, projectID, 3)
		if werr != nil {
			return nil, 0, false, pagesFetched, firstPageOrder, fmt.Errorf("real wheel scroll (attempt %d): %w", attempt, werr)
		}
		printRealWheelResult(attempt, wheelResult)
		time.Sleep(500 * time.Millisecond)

		if done, convs, dups, herr := tryAccumulate(idBase + 5 + attempt*2); done {
			if herr != nil {
				return nil, 0, false, pagesFetched, firstPageOrder, herr
			}
			return convs, dups, true, pagesFetched, firstPageOrder, nil
		}
	}
	return nil, 0, false, pagesFetched, firstPageOrder, fmt.Errorf("exhausted %d real-wheel attempts without observing a terminal cursor", maxWheelAttempts)
}

// projectScrollRegion mirrors bridge/preload.js's findProjectScrollRegion result: the scrollable
// container currently judged most likely to be the Project's own conversation list, selected by
// link counts/href shape/dimensions only — never link text or title.
type projectScrollRegion struct {
	Found                    bool `json:"found"`
	CandidateCount           int  `json:"candidateCount"`
	ConversationLinks        int  `json:"conversationLinks"`
	ProjectConversationLinks int  `json:"projectConversationLinks"`
	ScrollTop                int  `json:"scrollTop"`
	ScrollHeight             int  `json:"scrollHeight"`
	ClientHeight             int  `json:"clientHeight"`
}

func fetchProjectScrollRegion(client net.Conn, id int) (projectScrollRegion, error) {
	raw, err := call(client, request{ID: id, Method: "projectScrollRegion"})
	if err != nil {
		return projectScrollRegion{}, err
	}
	var result projectScrollRegion
	if err := json.Unmarshal(raw, &result); err != nil {
		return projectScrollRegion{}, fmt.Errorf("decode project scroll region: %w", err)
	}
	return result, nil
}

// wheelTickDiagnostic is one before/after observation within a single realWheelScrollProject call.
// Pointer fields distinguish "the region was not found for this tick" (nil) from a genuine zero.
type wheelTickDiagnostic struct {
	Index                          int  `json:"index"`
	ScrollTopBefore                *int `json:"scrollTopBefore"`
	ScrollTopAfter                 *int `json:"scrollTopAfter"`
	ProjectConversationLinksBefore *int `json:"projectConversationLinksBefore"`
	ProjectConversationLinksAfter  *int `json:"projectConversationLinksAfter"`
}

// realWheelScrollResult mirrors the bridge's realWheelScrollProject response: real Electron
// mouseWheel input sent to the ChatGPT WebContents (README "New hypothesis" — a synthetic DOM
// scroll event may not reproduce whatever browser-level input-pipeline behavior the real
// frontend's pagination trigger depends on), never a self-issued fetch of the cursor endpoint.
type realWheelScrollResult struct {
	Found          bool                  `json:"found"`
	CandidateCount int                   `json:"candidateCount"`
	WindowFocused  *bool                 `json:"windowFocused"`
	Initial        projectScrollRegion   `json:"initial"`
	Ticks          []wheelTickDiagnostic `json:"ticks"`
	Final          projectScrollRegion   `json:"final"`
}

func fetchRealWheelScrollProject(client net.Conn, id int, projectID string, ticks int) (realWheelScrollResult, error) {
	raw, err := call(client, request{ID: id, Method: "realWheelScrollProject", ProjectID: projectID, Params: map[string]any{"ticks": ticks}})
	if err != nil {
		return realWheelScrollResult{}, err
	}
	var result realWheelScrollResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return realWheelScrollResult{}, fmt.Errorf("decode real wheel scroll result: %w", err)
	}
	return result, nil
}

// cursorResponseStatusCounts mirrors the bridge's privacy-safe response-status tally (README Task
// 3): every real, observed response to the project-scoped conversations endpoint, bucketed by
// whether the request carried a `cursor` query parameter and by HTTP status. Never carries a URL,
// cursor value, header, or response body.
type cursorResponseStatusCounts struct {
	NoCursor      map[string]int `json:"noCursor"`
	CursorPresent map[string]int `json:"cursorPresent"`
}

func fetchProjectConversationsResponseStatus(client net.Conn, id int) (cursorResponseStatusCounts, error) {
	raw, err := call(client, request{ID: id, Method: "projectConversationsResponseStatus"})
	if err != nil {
		return cursorResponseStatusCounts{}, err
	}
	var result cursorResponseStatusCounts
	if err := json.Unmarshal(raw, &result); err != nil {
		return cursorResponseStatusCounts{}, fmt.Errorf("decode response status counts: %w", err)
	}
	return result, nil
}

func printProjectScrollRegion(label string, r projectScrollRegion) {
	if !r.Found {
		fmt.Printf("%s: found=false candidate_count=%d\n", label, r.CandidateCount)
		return
	}
	fmt.Printf("%s: found=true candidate_count=%d project_conversation_links=%d conversation_links=%d client_height=%d scroll_height=%d scroll_top=%d\n",
		label, r.CandidateCount, r.ProjectConversationLinks, r.ConversationLinks, r.ClientHeight, r.ScrollHeight, r.ScrollTop)
}

func intOrNil(p *int) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *p)
}

func boolOrNil(p *bool) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%t", *p)
}

func printRealWheelResult(attempt int, w realWheelScrollResult) {
	if !w.Found {
		fmt.Printf("real wheel (attempt %d): NOT TRIGGERED (no scroll target found, candidate_count=%d)\n", attempt, w.CandidateCount)
		return
	}
	fmt.Printf("real wheel (attempt %d): window_focused=%s\n", attempt, boolOrNil(w.WindowFocused))
	for _, tick := range w.Ticks {
		fmt.Printf("real wheel (attempt %d, tick %d): scroll_top_before=%s scroll_top_after=%s project_links_before=%s project_links_after=%s\n",
			attempt, tick.Index, intOrNil(tick.ScrollTopBefore), intOrNil(tick.ScrollTopAfter),
			intOrNil(tick.ProjectConversationLinksBefore), intOrNil(tick.ProjectConversationLinksAfter))
	}
}

func printResponseStatusCounts(counts cursorResponseStatusCounts) {
	fmt.Printf("cursor response status: no-cursor 200=%d 401=%d other=%d | cursor-present 200=%d 401=%d other=%d\n",
		counts.NoCursor["200"], counts.NoCursor["401"], counts.NoCursor["other"],
		counts.CursorPresent["200"], counts.CursorPresent["401"], counts.CursorPresent["other"])
}

// cursorExperimentOutcome is the five-way failure/success classification the task requires
// (README "Failure categories"), so a caller never has to infer which situation occurred from a
// bare error string.
type cursorExperimentOutcome string

const (
	// outcomeNoRequest: the wheel input never caused the real ChatGPT frontend to issue ANY cursor
	// endpoint request at all (neither a successful page nor a 401) — a wheel-trigger failure, NOT
	// an authorization failure. Must never be confused with outcomeFrontend401.
	outcomeNoRequest cursorExperimentOutcome = "NO_REQUEST"
	// outcomeFrontend401: the real frontend DID issue at least one cursor-present request, and
	// every one observed came back 401, with zero successful (200) cursor-present pages ever
	// captured — new architectural evidence distinct from the already-known, no-longer-executed
	// self-fetch 401.
	outcomeFrontend401 cursorExperimentOutcome = "FRONTEND_401"
	// outcomeCaptureFailure: a network response was captured, but body/schema validation
	// (accumulateCursorChain, parseCursorWireItems, dedupeCapturesByCursorIn) failed closed.
	outcomeCaptureFailure cursorExperimentOutcome = "CAPTURE_FAILURE"
	// outcomeChainIncomplete: pages were captured with no error, but no terminal cursor was
	// observed within the bounded number of wheel attempts.
	outcomeChainIncomplete cursorExperimentOutcome = "CHAIN_INCOMPLETE"
	// outcomeComplete: a terminal cursor was reached with no error anywhere in the chain.
	outcomeComplete cursorExperimentOutcome = "COMPLETE"
)

// classifyCursorExperimentOutcome is the pure decision core behind the five failure/success
// categories (README "Failure categories"), evaluated in the priority order the task specifies:
// an total absence of any cursor-present traffic (success or failure) means the wheel input never
// triggered a request at all, which is a distinct, more basic problem than an authorization
// failure and must be reported as such rather than silently falling through to a later category.
func classifyCursorExperimentOutcome(cursorPresent200, cursorPresent401 int, pagesCaptured int, chainErr error, complete bool) cursorExperimentOutcome {
	switch {
	case cursorPresent200 == 0 && cursorPresent401 == 0 && pagesCaptured == 0:
		return outcomeNoRequest
	case cursorPresent200 == 0 && cursorPresent401 > 0:
		return outcomeFrontend401
	case chainErr != nil:
		return outcomeCaptureFailure
	case !complete:
		return outcomeChainIncomplete
	default:
		return outcomeComplete
	}
}

// knownSampleFingerprintsResult reports SHA-256 fingerprints (never raw IDs) of one already-known
// Work-marked conversation and one already-known plain conversation (README Phase E), reusing the
// exact same bridge-side selection the existing openURLProbe method already performs
// (bridge/main.js's payloadHasAsyncSource over capturedConversationDetails) rather than
// reimplementing or resolving the Chat/Work discriminator itself — this spike's scope is List
// completeness only (README "Important" section). A nil field means no such sample has been
// captured yet in this run (e.g. conversationEvidence was never called, or found no async_source
// sample).
type knownSampleFingerprintsResult struct {
	WorkFingerprint *string `json:"workFingerprint"`
	ChatFingerprint *string `json:"chatFingerprint"`
}

func fetchKnownSampleFingerprints(client net.Conn, id int) (knownSampleFingerprintsResult, error) {
	raw, err := call(client, request{ID: id, Method: "knownSampleFingerprints"})
	if err != nil {
		return knownSampleFingerprintsResult{}, err
	}
	var result knownSampleFingerprintsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return knownSampleFingerprintsResult{}, fmt.Errorf("decode known sample fingerprints: %w", err)
	}
	return result, nil
}

// runCursorExperiment drives README Phases B through G of the Project-scoped cursor pagination
// spike end to end and prints the diagnostic report the task requires. oldProjectScopedCount is
// the count from the existing no-cursor `conversations` call made earlier in the same run
// (README Phase D); oldGlobalFilteredCount/haveOldGlobalFilteredCount is the previously-measured
// global-endpoint Project-filtered count, when that call succeeded in the same run, for the same
// comparison. It never itself declares completeness beyond what
// enumerateProjectConversationsByCursor/accumulateCursorChain establish (README "Completeness
// criteria" / "Do not use total as completeness proof").
func runCursorExperiment(client net.Conn, projectID string, oldProjectScopedCount int, oldGlobalFilteredCount int, haveOldGlobalFilteredCount bool) error {
	fmt.Println("=== Project cursor pagination experiment (Issue #7 spike) ===")
	// The self-fetch path is a confirmed, historical negative (HTTP 401 once a `cursor` parameter
	// is present — see README) and is no longer executed by the normal experiment (README Task 2):
	// running it every time added a misleading 401 to every report without adding new evidence,
	// and made it hard to tell a "known, expected" 401 apart from a genuinely new one on the real
	// frontend's own request path. Run with `-cursor-self-fetch-probe` to re-verify it independently.
	fmt.Println("known self-fetch probe: skipped (known negative: HTTP 401 once a cursor parameter is present; use -cursor-self-fetch-probe to re-verify)")

	conversations, duplicates, complete, pagesFetched, firstPageOrder, enumErr := enumerateProjectConversationsByCursorPassive(client, projectID, 8, 50, 700)

	statusCounts, serr := fetchProjectConversationsResponseStatus(client, 780)
	cursorPresent200, cursorPresent401 := 0, 0
	if serr != nil {
		fmt.Printf("cursor response status: NOT VERIFIED (%v)\n", serr)
	} else {
		printResponseStatusCounts(statusCounts)
		cursorPresent200 = statusCounts.CursorPresent["200"]
		cursorPresent401 = statusCounts.CursorPresent["401"]
	}

	outcome := classifyCursorExperimentOutcome(cursorPresent200, cursorPresent401, pagesFetched, enumErr, complete)
	fmt.Printf("failure category: %s\n", outcome)

	if enumErr != nil {
		fmt.Printf("Project session enumeration: FAIL (%v)\n", enumErr)
		return nil
	}

	fmt.Printf("pages fetched: %d\n", pagesFetched)
	fmt.Printf("terminal cursor observed: %t\n", complete)
	fmt.Printf("unique conversations: %d duplicates_observed: %d\n", len(conversations), duplicates)
	if complete {
		fmt.Println("Project session enumeration: COMPLETE")
	} else {
		fmt.Println("Project session enumeration: INCOMPLETE")
	}

	fmt.Printf("response ordering (first page, server order): created_desc=%t updated_desc=%t\n",
		isNonIncreasingByCreated(firstPageOrder), isNonIncreasingByUpdated(firstPageOrder))

	sorted := sortConversationsByCreatedDesc(conversations)
	fmt.Printf("agentsctl ordering (creation time DESC) verified: %t\n", isNonIncreasingByCreated(sorted))

	known, kerr := fetchKnownSampleFingerprints(client, 750)
	if kerr != nil {
		fmt.Printf("known Chat/Work inclusion check: NOT VERIFIED (%v)\n", kerr)
	} else {
		present := make(map[string]bool, len(conversations))
		for _, c := range conversations {
			present[conversationFingerprint(c.ID)] = true
		}
		reportPresence := func(label string, fingerprint *string) {
			if fingerprint == nil {
				fmt.Printf("known %s present: not verified (no captured sample)\n", label)
				return
			}
			fmt.Printf("known %s present: %t\n", label, present[*fingerprint])
		}
		reportPresence("normal Chat", known.ChatFingerprint)
		reportPresence("Work", known.WorkFingerprint)
	}

	fmt.Printf("comparison vs old project-scoped (no cursor) count=%d: cursor_set_count=%d (>= old: %t)\n",
		oldProjectScopedCount, len(conversations), len(conversations) >= oldProjectScopedCount)
	if haveOldGlobalFilteredCount {
		fmt.Printf("comparison vs old global-filtered subset count=%d: cursor_set_count=%d (>= old: %t)\n",
			oldGlobalFilteredCount, len(conversations), len(conversations) >= oldGlobalFilteredCount)
	} else {
		fmt.Println("comparison vs old global-filtered subset: NOT AVAILABLE this run")
	}

	return nil
}
