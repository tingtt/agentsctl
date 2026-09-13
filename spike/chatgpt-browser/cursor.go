package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
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
// a bridge-internal arrival-order reference, never a pagination position — CursorIn (extracted
// bridge-side from the real request's own `cursor` query parameter, or "0" if the request omitted
// it entirely, e.g. the endpoint's very first natural request) is the real pagination identity.
type cursorCaptureWireItem struct {
	CaptureID     int                          `json:"captureID"`
	CursorIn      string                       `json:"cursorIn"`
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

// dedupeCapturesByCursorIn collapses passively-observed captures (which can include a benign
// duplicate — e.g. a React re-render re-issuing the exact same request) down to one page per
// distinct CursorIn value, keeping the first-seen (lowest CaptureID, i.e. earliest arrival) and
// requiring every later observation of the SAME CursorIn to agree on content (item count and
// declared next cursor). A disagreement is treated as schema drift or genuine mid-enumeration
// account activity, not a benign duplicate, and fails closed rather than silently picking one.
func dedupeCapturesByCursorIn(captures []cursorCaptureWireItem) ([]cursorCaptureWireItem, error) {
	ordered := make([]cursorCaptureWireItem, len(captures))
	copy(ordered, captures)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CaptureID < ordered[j].CaptureID })
	seen := make(map[string]cursorCaptureWireItem, len(ordered))
	order := make([]string, 0, len(ordered))
	for _, c := range ordered {
		prev, ok := seen[c.CursorIn]
		if !ok {
			seen[c.CursorIn] = c
			order = append(order, c.CursorIn)
			continue
		}
		if prev.HasNextCursor != c.HasNextCursor || prev.NextCursor != c.NextCursor || prev.RawItemCount != c.RawItemCount {
			return nil, fmt.Errorf("cursor %s was observed twice with different content (schema drift or account activity mid-enumeration)", redactedCursor(c.CursorIn))
		}
	}
	result := make([]cursorCaptureWireItem, 0, len(order))
	for _, cursorIn := range order {
		result = append(result, seen[cursorIn])
	}
	return result, nil
}

// enumerateProjectConversationsByCursorPassive is the primary enumeration mechanism (see the
// self-fetch 401 evidence on enumerateProjectConversationsByCursorSelfFetch above): it navigates
// into the Project's own view (which is what naturally issues this endpoint's first real request),
// then repeatedly harvests whatever the real ChatGPT client has passively been observed
// requesting, running accumulateCursorChain after every harvest — exactly the same pure,
// unit-tested logic exercised directly by cursor_test.go — BEFORE attempting another scroll
// simulation. If no terminal cursor is reached within maxScrollAttempts, this returns an error
// (never a partial result presented as complete), mirroring every other fail-closed enumeration in
// this spike.
func enumerateProjectConversationsByCursorPassive(client net.Conn, projectID string, maxScrollAttempts, maxPages, idBase int) (conversations []cursorConversation, duplicatesObserved int, complete bool, pagesFetched int, firstPageOrder []cursorConversation, err error) {
	if _, err := call(client, request{ID: idBase, Method: "navigateProject", ProjectID: projectID}); err != nil {
		return nil, 0, false, 0, nil, fmt.Errorf("navigate to Project view: %w", err)
	}
	time.Sleep(2 * time.Second)

	harvest := func() (pages []cursorFetchedPage, firstOrder []cursorConversation, err error) {
		raw, err := fetchCursorCaptures(client, idBase+1, projectID)
		if err != nil {
			return nil, nil, err
		}
		deduped, err := dedupeCapturesByCursorIn(raw)
		if err != nil {
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

	for attempt := 0; attempt <= maxScrollAttempts; attempt++ {
		pages, firstOrder, herr := harvest()
		if herr != nil {
			return nil, 0, false, len(pages), firstOrder, herr
		}
		if len(pages) > 0 {
			for i, p := range pages {
				outLabel := "<terminal>"
				if p.HasNextCursor {
					outLabel = redactedCursor(p.NextCursor)
				}
				fmt.Printf("page=%d conversation_count=%d cursor_in=%s cursor_out=%s\n", i, len(p.Conversations), redactedCursor(p.CursorIn), outLabel)
			}
			convs, dups, comp, aerr := accumulateCursorChain(pages, maxPages)
			if aerr != nil {
				return nil, 0, false, len(pages), firstOrder, aerr
			}
			if comp {
				return convs, dups, true, len(pages), firstOrder, nil
			}
			pagesFetched, firstPageOrder = len(pages), firstOrder
		}
		if attempt == maxScrollAttempts {
			break
		}
		scrollRaw, serr := call(client, request{ID: idBase + 2 + attempt, Method: "simulateSidebarScroll"})
		if serr != nil {
			return nil, 0, false, pagesFetched, firstPageOrder, fmt.Errorf("simulate scroll (attempt %d): %w", attempt, serr)
		}
		var scroll sidebarScrollResult
		if err := json.Unmarshal(scrollRaw, &scroll); err != nil {
			return nil, 0, false, pagesFetched, firstPageOrder, fmt.Errorf("decode scroll result (attempt %d): %w", attempt, err)
		}
		printScrollDiagnostics(attempt, scroll)
		time.Sleep(1500 * time.Millisecond)
	}
	return nil, 0, false, pagesFetched, firstPageOrder, fmt.Errorf("exhausted %d scroll attempts without observing a terminal cursor", maxScrollAttempts)
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

	if _, _, _, _, _, err := enumerateProjectConversationsByCursorSelfFetch(client, projectID, 1, 799); err != nil {
		fmt.Printf("self-initiated fetch of cursor-paginated endpoint: NOT AUTHORIZED (%v)\n", err)
	} else {
		fmt.Println("self-initiated fetch of cursor-paginated endpoint: PASS")
	}

	conversations, duplicates, complete, pagesFetched, firstPageOrder, err := enumerateProjectConversationsByCursorPassive(client, projectID, 8, 50, 700)
	if err != nil {
		fmt.Printf("Project session enumeration: FAIL (%v)\n", err)
		return nil
	}

	fmt.Printf("pages fetched: %d\n", pagesFetched)
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
