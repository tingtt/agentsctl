// Command chatgpt-browser proves the process and security boundaries proposed by issue #7.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const protocolVersion = 1

type config struct {
	binary        string
	partition     string
	url           string
	socket        string
	projectID     string
	projectName   string
	hold          time.Duration
	closePTYAfter time.Duration
}

type request struct {
	ID                   int            `json:"id"`
	Method               string         `json:"method"`
	ProjectID            string         `json:"projectID,omitempty"`
	ConversationIDs      []string       `json:"conversationIDs,omitempty"`
	Params               map[string]any `json:"params,omitempty"`
	CaptureID            int            `json:"captureID,omitempty"`
	KnownConversationIDs []string       `json:"knownConversationIDs,omitempty"`
}

type queryParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// globalConversationsPageDiagnostic mirrors one bridge-side capture record. CaptureID is a
// bridge-internal reference to this one observed response, assigned once and never reused — it
// carries no pagination meaning. SeriesKey/Offset are the actual pagination identity, computed by
// the bridge from the response's own query and metadata. SeriesKey is a digest of the RAW,
// unredacted query (see bridge/main.js's canonicalSeriesKeyFrom) — it is NOT derived from the
// redacted Query diagnostic below, which exists purely for human-readable display and must never be
// used to decide whether two captures share a pagination series (two different opaque values of the
// same length redact to the same placeholder and would otherwise collide). Offset/Limit are
// pointers because a response missing usable pagination metadata must be distinguishable from one
// legitimately at offset 0. RecognizedCollection distinguishes a response whose top-level item
// collection could actually be located (even if empty) from one whose shape wasn't recognized at
// all — the latter must never be treated as "0 items".
type globalConversationsPageDiagnostic struct {
	CaptureID            int            `json:"captureID"`
	SeriesKey            *string        `json:"seriesKey"`
	Query                []queryParam   `json:"query"`
	HideSnorlax          bool           `json:"hideSnorlax"`
	IsArchived           *bool          `json:"isArchived"`
	IsStarred            *bool          `json:"isStarred"`
	Order                *string        `json:"order"`
	HasUnknownParameters bool           `json:"hasUnknownParameters"`
	Offset               *int           `json:"offset"`
	Limit                *int           `json:"limit"`
	RecognizedCollection bool           `json:"recognizedCollection"`
	RawItemCount         int            `json:"rawItemCount"`
	RecognizedIDCount    int            `json:"recognizedIDCount"`
	RawIdentityDigest    string         `json:"rawIdentityDigest"`
	TopLevelKeys         []string       `json:"topLevelKeys"`
	Meta                 map[string]any `json:"meta"`
}

// globalConversationsCaptureItemsResult is the bridge's response to fetching one capture's
// sanitized, Project-filtered items, along with cross-validation of a caller-supplied allowlist of
// already-known Project conversation IDs (KnownIDsMismatched) and a universal per-item association
// shape check (UnrecognizedAssociationCount) — see mergeProjectPages and the sanitizer comment in
// bridge/preload.js for what each can and cannot prove.
type globalConversationsCaptureItemsResult struct {
	Items                        []conversation `json:"items"`
	KnownIDsSeen                 int            `json:"knownIDsSeen"`
	KnownIDsMismatched           int            `json:"knownIDsMismatched"`
	UnrecognizedAssociationCount int            `json:"unrecognizedAssociationCount"`
}

// conversationPage is one page of the Project-inclusive (hide_snorlax false/absent) view of
// /backend-api/conversations, already validated shape-wise and Project-filtered by the bridge.
// SeriesKey+Offset is its pagination identity — NOT any capture/arrival-order index — so that
// pages can be assembled into a contiguous offset chain regardless of the order they were observed
// in, and so that two different filter combinations (e.g. differing is_archived, or a limit change)
// are never treated as continuations of the same series.
type conversationPage struct {
	SeriesKey                    string
	HideSnorlax                  bool
	IsArchived                   *bool
	IsStarred                    *bool
	HasUnknownParameters         bool
	Offset                       int
	Limit                        int
	RecognizedCollection         bool
	RawItemCount                 int
	RecognizedIDCount            int
	RawIdentityDigest            string
	KnownIDsMismatched           int
	UnrecognizedAssociationCount int
	Items                        []conversation
}

// mergeProjectPages is the deterministic core of Project session enumeration. It groups pages by
// SeriesKey (a digest of the RAW query with `offset` removed — see canonicalSeriesKeyFrom in
// bridge/main.js; never derived from redacted diagnostics, which could collide two different opaque
// values of the same length), and within each series walks a contiguous offset chain starting at 0
// and stepping by that series' own limit. A series is exhausted only when that chain reaches a page
// whose raw item count is strictly less than its limit — never from the endpoint's own `total`
// field (observed to grow between successive calls in the same session), and never from a full
// first page alone (a page exactly as long as its limit does not, by itself, prove no further page
// exists). The overall result is exhausted only if every observed series is individually exhausted;
// a gap in one series' offset chain, or a second, unrelated series appearing at a
// numerically-contiguous-looking offset, can never manufacture a false COMPLETE.
//
// Item merging (for the returned, possibly-partial conversation list) and identity/schema
// validation happen over ALL captured pages regardless of chain contiguity, since a gap in one
// series' pagination shouldn't discard conversations legitimately observed elsewhere. Re-observing
// the same SeriesKey+Offset is compared by RawIdentityDigest — a fingerprint of the RAW page's own
// conversation identity, not the Project-filtered subset, so two raw pages that happen to agree on
// their filtered subset while actually differing underneath still fail closed.
//
// It fails closed (returns an error, never a partial result presented as complete) on: a
// hide_snorlax-excluding page reaching it, a missing SeriesKey, an unrecognized item-collection
// shape (RecognizedCollection == false — never treated as an empty page), more pages than maxPages,
// a malformed page (non-positive limit or negative/unrecognized offset), a raw item whose identity
// couldn't be recognized (RecognizedIDCount < RawItemCount), a raw item exposing no recognizable
// Project-association field at all (UnrecognizedAssociationCount > 0 — see the sanitizer comment in
// bridge/preload.js for exactly what this can and cannot prove), a known Project conversation ID
// that no longer resolves to the configured Project (KnownIDsMismatched > 0), a conversation missing
// a stable ID, or the same SeriesKey+Offset reporting a different raw page identity across
// observations.
func mergeProjectPages(pages []conversationPage, maxPages int) (conversations []conversation, exhausted bool, err error) {
	if len(pages) > maxPages {
		return nil, false, fmt.Errorf("excessive page count: got %d pages, max %d", len(pages), maxPages)
	}
	type seriesState struct {
		byOffset map[int]conversationPage
	}
	series := make(map[string]*seriesState)
	for _, page := range pages {
		if page.HideSnorlax {
			return nil, false, errors.New("malformed input: a hide_snorlax-excluding page cannot be merged into Project enumeration")
		}
		if page.SeriesKey == "" {
			return nil, false, errors.New("malformed page: missing pagination series identity")
		}
		if !page.RecognizedCollection {
			return nil, false, fmt.Errorf("schema drift (series=%q offset=%d): response's item collection shape was not recognized — an unrecognized collection must never be treated as an empty page",
				page.SeriesKey, page.Offset)
		}
		if page.Limit <= 0 {
			return nil, false, fmt.Errorf("malformed page (series=%q offset=%d): non-positive limit %d", page.SeriesKey, page.Offset, page.Limit)
		}
		if page.Offset < 0 {
			return nil, false, fmt.Errorf("malformed page (series=%q): negative or unrecognized offset %d", page.SeriesKey, page.Offset)
		}
		if page.RawItemCount > 0 && page.RecognizedIDCount < page.RawItemCount {
			return nil, false, fmt.Errorf("schema drift (series=%q offset=%d): %d of %d raw items had an unrecognizable identity",
				page.SeriesKey, page.Offset, page.RecognizedIDCount, page.RawItemCount)
		}
		if page.UnrecognizedAssociationCount > 0 {
			return nil, false, fmt.Errorf("schema drift (series=%q offset=%d): %d raw item(s) exposed no recognizable Project-association field at all",
				page.SeriesKey, page.Offset, page.UnrecognizedAssociationCount)
		}
		if page.KnownIDsMismatched > 0 {
			return nil, false, fmt.Errorf("schema drift (series=%q offset=%d): %d known Project conversation(s) no longer resolve to the configured Project",
				page.SeriesKey, page.Offset, page.KnownIDsMismatched)
		}
		for _, item := range page.Items {
			if item.ID == "" {
				return nil, false, fmt.Errorf("malformed page (series=%q offset=%d): conversation missing stable identity", page.SeriesKey, page.Offset)
			}
		}
		st, ok := series[page.SeriesKey]
		if !ok {
			st = &seriesState{byOffset: make(map[int]conversationPage)}
			series[page.SeriesKey] = st
		}
		if previous, ok := st.byOffset[page.Offset]; ok {
			if previous.RawIdentityDigest != page.RawIdentityDigest {
				return nil, false, fmt.Errorf("series %q offset %d reported a different raw page identity across observations", page.SeriesKey, page.Offset)
			}
		} else {
			st.byOffset[page.Offset] = page
		}
	}
	if len(series) == 0 {
		return nil, false, nil
	}
	byID := make(map[string]conversation)
	allExhausted := true
	for _, st := range series {
		for _, page := range st.byOffset {
			for _, item := range page.Items {
				byID[item.ID] = item
			}
		}
		offset := 0
		exhaustedThis := false
		for {
			page, ok := st.byOffset[offset]
			if !ok {
				break
			}
			if page.RawItemCount < page.Limit {
				exhaustedThis = true
				break
			}
			offset += page.Limit
		}
		if !exhaustedThis {
			allExhausted = false
		}
	}
	result := make([]conversation, 0, len(byID))
	for _, item := range byID {
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b conversation) int { return strings.Compare(a.ID, b.ID) })
	return result, allExhausted, nil
}

// seriesRequirement is one required pagination-series shape for a target session universe (see
// evaluateTargetPagination). A nil field means "don't care about this dimension"; a non-nil field
// requires an exact, confirmed match — a page whose corresponding descriptor field is nil (unknown/
// unrecognized value) never satisfies a non-nil requirement, so uncertainty is never silently
// treated as a match.
type seriesRequirement struct {
	RequireIsArchived       *bool
	RequireIsStarred        *bool
	ForbidUnknownParameters bool
}

func boolPtr(v bool) *bool { return &v }

func (req seriesRequirement) matches(p conversationPage) bool {
	if p.HideSnorlax {
		return false
	}
	if req.ForbidUnknownParameters && p.HasUnknownParameters {
		return false
	}
	if req.RequireIsArchived != nil && (p.IsArchived == nil || *p.IsArchived != *req.RequireIsArchived) {
		return false
	}
	if req.RequireIsStarred != nil && (p.IsStarred == nil || *p.IsStarred != *req.RequireIsStarred) {
		return false
	}
	return true
}

// completenessResult separates "definitely not complete" from "we don't know if our target
// definition is even right" — see evaluateActiveListCompleteness. Status is one of "COMPLETE",
// "INCOMPLETE", or "UNKNOWN".
type completenessResult struct {
	Status string
	Reason string
}

// evaluateTargetPagination is the coverage layer's pure pagination-axis evaluation: pagination
// completeness of an offset chain (mergeProjectPages) says nothing about which chain(s) actually
// represent the session universe a caller needs. This partitions the observed pages by which
// `required` series shape(s) they match — a page matching NONE of them is irrelevant to this target
// universe and is dropped entirely before it ever reaches mergeProjectPages, so an irrelevant
// series (even a schema-broken one) can never influence the result — then requires EVERY listed
// requirement to be independently observed and pagination-exhausted for the overall result to be
// exhausted.
//
// This never itself returns "unknown": that judgment — whether `required` correctly captures the
// full intended universe in the first place — is a separate, higher-level question answered by the
// caller (see evaluateActiveListCompleteness and the README's Phase A/B/C discussion of the
// is_starred ambiguity).
func evaluateTargetPagination(pages []conversationPage, required []seriesRequirement, maxPages int) (conversations []conversation, exhausted bool, err error) {
	if len(required) == 0 {
		return nil, false, nil
	}
	byID := make(map[string]conversation)
	allSatisfied := true
	for _, req := range required {
		var matched []conversationPage
		for _, p := range pages {
			if req.matches(p) {
				matched = append(matched, p)
			}
		}
		items, reqExhausted, err := mergeProjectPages(matched, maxPages)
		if err != nil {
			return nil, false, err
		}
		for _, item := range items {
			byID[item.ID] = item
		}
		if !reqExhausted {
			allSatisfied = false
		}
	}
	result := make([]conversation, 0, len(byID))
	for _, item := range byID {
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b conversation) int { return strings.Compare(a.ID, b.ID) })
	return result, allSatisfied, nil
}

// activeListRequiredSeries is this PoC's live-evidence-based definition of the target series
// required for the "active, non-archived Project session" universe — the scope of the existing
// production session.Provider.List(archived bool) called with archived=false (see README Phase A).
// It is exactly the query shape the real ChatGPT UI's own default view was observed sending:
// hide_snorlax=false (Project-inclusive; enforced structurally, not via this requirement — see
// seriesRequirement.matches), is_archived=false, is_starred=false, and no unrecognized query
// parameter.
var activeListRequiredSeries = []seriesRequirement{
	{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(false), ForbidUnknownParameters: true},
}

// activeListCoverageCaveat documents the one dimension this PoC could not confirm or rule out: see
// README Phase B/C.
const activeListCoverageCaveat = "is_starred semantics unconfirmed: no real ChatGPT UI action in this session ever requested is_starred=true, so a possible separate required series for starred/pinned conversations cannot be ruled in or out; indirect evidence (a separate /backend-api/pins endpoint observed in real traffic, independent of /backend-api/conversations) suggests pin/star state does not partition the conversations list, but this was not directly tested"

// evaluateActiveListCompleteness is the top-level completeness judgement for this PoC's target
// universe. It reports Pagination (did the required series reach exhaustion) and Coverage (is the
// required-series definition itself trustworthy as the FULL intended universe) as two independent
// axes — see the README's Phase A/B/C for why Coverage is UNKNOWN, not COMPLETE, even when
// Pagination succeeds: this account never exercised an is_starred=true query, so whether it
// represents a separate required bucket was never tested. Overall completeness (decided by the
// caller) should require BOTH axes to be COMPLETE.
func evaluateActiveListCompleteness(pages []conversationPage, maxPages int) (conversations []conversation, pagination completenessResult, coverage completenessResult, err error) {
	items, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, maxPages)
	if err != nil {
		return nil, completenessResult{}, completenessResult{}, err
	}
	if exhausted {
		pagination = completenessResult{Status: "COMPLETE", Reason: "the required active-Project series reached a contiguous, pagination-exhausted offset chain"}
	} else {
		pagination = completenessResult{Status: "INCOMPLETE", Reason: "the required active-Project series was not observed at all, or not yet pagination-exhausted"}
	}
	coverage = completenessResult{Status: "UNKNOWN", Reason: activeListCoverageCaveat}
	return items, pagination, coverage, nil
}

type globalConversationsPageResult struct {
	Items        []conversation `json:"items"`
	RawItemCount int            `json:"rawItemCount"`
	Meta         map[string]any `json:"meta"`
}

type sidebarScrollResult struct {
	Triggered                   bool   `json:"triggered"`
	Reason                      string `json:"reason"`
	ContainerCount              int    `json:"containerCount"`
	ConversationLinkCountBefore int    `json:"conversationLinkCountBefore"`
	ConversationLinkCountAfter  int    `json:"conversationLinkCountAfter"`
}

type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type pingInfo struct {
	AttachedDebuggerCount           int  `json:"attachedDebuggerCount"`
	MatchedResponseCount            int  `json:"matchedResponseCount"`
	CaptureErrorCount               int  `json:"captureErrorCount"`
	ProjectsCaptured                bool `json:"projectsCaptured"`
	ConversationCollectionsCaptured int  `json:"conversationCollectionsCaptured"`
}

type pageInfo struct {
	Href                  string   `json:"href"`
	Title                 string   `json:"title"`
	ReadyState            string   `json:"readyState"`
	LoginPromptVisible    bool     `json:"loginPromptVisible"`
	AccountControlPresent bool     `json:"accountControlPresent"`
	ConversationLinkCount int      `json:"conversationLinkCount"`
	ProjectLinkCount      int      `json:"projectLinkCount"`
	ObservedBackendPaths  []string `json:"observedBackendPaths"`
}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type conversation struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	ProjectID     string         `json:"projectID"`
	Discriminator map[string]any `json:"discriminator"`
	AvailableKeys []string       `json:"availableKeys"`
}

type conversationEvidence struct {
	Path               string   `json:"path"`
	PresentCount       int      `json:"presentCount"`
	DistinctValueCount int      `json:"distinctValueCount"`
	Values             []string `json:"values"`
	Types              []string `json:"types"`
}

type urlProbeResult struct {
	Style                   string `json:"style"`
	ReadyState              string `json:"readyState"`
	LoginPromptVisible      bool   `json:"loginPromptVisible"`
	HrefMatchesExpectedPath bool   `json:"hrefMatchesExpectedPath"`
	RedactedHref            string `json:"redactedHref"`
	RedactedExpectedPath    string `json:"redactedExpectedPath"`
}

type openURLProbeResult struct {
	WorkLikeCanonical     urlProbeResult `json:"workLikeCanonical"`
	WorkLikeProjectScoped urlProbeResult `json:"workLikeProjectScoped"`
	ChatLikeCanonical     urlProbeResult `json:"chatLikeCanonical"`
	ChatLikeProjectScoped urlProbeResult `json:"chatLikeProjectScoped"`
}

type taskEvidence struct {
	ConversationIDMatches         int      `json:"conversationIDMatches"`
	OverlapWithKnownConversations int      `json:"overlapWithKnownConversations"`
	StatusFieldCandidates         []string `json:"statusFieldCandidates"`
	DistinctStatusValueCount      int      `json:"distinctStatusValueCount"`
}

func main() {
	if err := run(context.Background(), parseFlags()); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.binary, "terminal-browser", "terminal-browser", "terminal-browser executable")
	flag.StringVar(&cfg.partition, "partition", "agentsctl-chatgpt", "persistent browser partition")
	flag.StringVar(&cfg.url, "url", "https://chatgpt.com", "initial ChatGPT URL")
	flag.StringVar(&cfg.socket, "socket", filepath.Join(os.TempDir(), "agentsctl-chatgpt-browser.sock"), "bridge socket")
	flag.StringVar(&cfg.projectID, "project-id", "", "Project ID to query")
	flag.StringVar(&cfg.projectName, "project-name", "", "Project name to resolve without printing names")
	flag.DurationVar(&cfg.hold, "hold", 0, "keep the helper alive after probes")
	flag.DurationVar(&cfg.closePTYAfter, "close-pty-after", 0, "close the PTY after this duration for lifecycle testing")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config) error {
	if runtime.GOOS == "windows" {
		return errors.New("the pseudo-PTY spike requires a Unix host")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	mainScript := filepath.Join(root, "bridge", "main.js")
	preload := filepath.Join(root, "bridge", "preload.js")
	for _, path := range []string{mainScript, preload} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("bridge asset %s: %w", path, err)
		}
	}

	cmd := exec.CommandContext(ctx, cfg.binary,
		"open", cfg.url,
		"--no-merge",
		"--partition="+cfg.partition,
		"--main-script="+mainScript,
		"--preload="+preload,
	)
	cmd.Env = append(os.Environ(),
		"TERMINAL_BROWSER_SKIP_GRAPHICS_CHECK=1",
		"AGENTSCTL_CHATGPT_BRIDGE_SOCKET="+cfg.socket,
	)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start terminal-browser: %w", err)
	}
	defer ptmx.Close()
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()

	if cfg.closePTYAfter > 0 {
		go func() {
			time.Sleep(cfg.closePTYAfter)
			_ = ptmx.Close()
		}()
	}

	client, err := dialBridge(ctx, cfg.socket, 20*time.Second)
	if err != nil {
		_ = terminate(cmd)
		return err
	}
	defer client.Close()

	if _, err := call(client, request{ID: 1, Method: "ping"}); err != nil {
		return err
	}
	fmt.Printf("bridge round trip: PASS (protocol %d)\n", protocolVersion)

	raw, err := call(client, request{ID: 2, Method: "pageInfo"})
	if err != nil {
		return err
	}
	var page pageInfo
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode pageInfo: %w", err)
	}
	fmt.Printf("page: origin=%s ready=%s login_prompt=%t account_control=%t conversation_links=%d project_links=%d title_present=%t\n",
		origin(page.Href), page.ReadyState, page.LoginPromptVisible, page.AccountControlPresent,
		page.ConversationLinkCount, page.ProjectLinkCount, page.Title != "")
	if err := client.Close(); err != nil {
		return fmt.Errorf("close first bridge connection: %w", err)
	}
	client, err = dialBridge(ctx, cfg.socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("reconnect bridge: %w", err)
	}
	defer client.Close()
	if _, err := call(client, request{ID: 8, Method: "ping"}); err != nil {
		return fmt.Errorf("bridge reconnect round trip: %w", err)
	}
	fmt.Println("bridge client reconnect: PASS")

	projectID := cfg.projectID
	if cfg.projectName != "" || projectID != "" {
		time.Sleep(3 * time.Second)
		raw, err = call(client, request{ID: 9, Method: "pageInfo"})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode settled pageInfo: %w", err)
		}
		fmt.Printf("settled page: ready=%s login_prompt=%t account_control=%t conversation_links=%d project_links=%d\n",
			page.ReadyState, page.LoginPromptVisible, page.AccountControlPresent,
			page.ConversationLinkCount, page.ProjectLinkCount)
		if len(page.ObservedBackendPaths) > 0 {
			fmt.Printf("observed redacted backend paths: %s\n", strings.Join(page.ObservedBackendPaths, ", "))
		}
		raw, err = call(client, request{ID: 10, Method: "ping"})
		if err != nil {
			return err
		}
		var capture pingInfo
		if err := json.Unmarshal(raw, &capture); err != nil {
			return fmt.Errorf("decode capture status: %w", err)
		}
		fmt.Printf("capture: debuggers=%d matched_responses=%d errors=%d projects=%t conversation_collections=%d\n",
			capture.AttachedDebuggerCount, capture.MatchedResponseCount, capture.CaptureErrorCount,
			capture.ProjectsCaptured, capture.ConversationCollectionsCaptured)
		raw, err = call(client, request{ID: 3, Method: "projects"})
		if err != nil {
			return fmt.Errorf("discover projects: %w", err)
		}
		var projects []project
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("decode projects: %w", err)
		}
		fmt.Printf("projects: PASS count=%d (names and IDs not logged)\n", len(projects))
		if projectID == "" {
			for _, candidate := range projects {
				if candidate.Name == cfg.projectName {
					if projectID != "" {
						return fmt.Errorf("Project name %q is ambiguous", cfg.projectName)
					}
					projectID = candidate.ID
				}
			}
			if projectID == "" {
				return fmt.Errorf("Project name %q was not found", cfg.projectName)
			}
		}
	}

	if projectID != "" {
		first, err := discoverConversations(client, 4, projectID)
		if err != nil {
			return err
		}
		second, err := discoverConversations(client, 5, projectID)
		if err != nil {
			return err
		}
		if !sameIDs(first, second) {
			return errors.New("conversation IDs changed between repeated discovery calls")
		}
		fmt.Printf("conversations: PASS count=%d repeated_ids_stable=true (titles and IDs not logged)\n", len(first))
		printDiscriminatorEvidence(first)
		ids := make([]string, 0, len(first))
		for _, item := range first {
			ids = append(ids, item.ID)
		}
		raw, err = call(client, request{ID: 11, Method: "conversationEvidence", ConversationIDs: ids})
		if err != nil {
			return fmt.Errorf("compare conversation evidence: %w", err)
		}
		var evidence []conversationEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return fmt.Errorf("decode conversation evidence: %w", err)
		}
		fmt.Printf("conversation detail evidence: fields=%d sessions=%d (values are structural marker labels only; conversation content, titles, and per-conversation identity mapping are never logged)\n",
			len(evidence), len(first))
		for _, field := range evidence {
			fmt.Printf("evidence field: path=%s present=%d distinct_values=%d values=%s types=%s\n",
				field.Path, field.PresentCount, field.DistinctValueCount, strings.Join(field.Values, "|"), strings.Join(field.Types, ","))
		}

		raw, err = call(client, request{ID: 12, Method: "tasks", ConversationIDs: ids})
		if err != nil {
			fmt.Printf("tasks: NOT VERIFIED (%v)\n", err)
		} else {
			var tasks taskEvidence
			if err := json.Unmarshal(raw, &tasks); err != nil {
				return fmt.Errorf("decode tasks: %w", err)
			}
			fmt.Printf("tasks: PASS conversation_id_like=%d overlap_with_project_conversations=%d status_fields=%s distinct_status_values=%d\n",
				tasks.ConversationIDMatches, tasks.OverlapWithKnownConversations,
				strings.Join(tasks.StatusFieldCandidates, ","), tasks.DistinctStatusValueCount)
		}

		raw, err = call(client, request{ID: 13, Method: "globalConversations", ProjectID: projectID})
		if err != nil {
			fmt.Printf("global conversations: NOT VERIFIED (%v)\n", err)
		} else {
			var global []conversation
			if err := json.Unmarshal(raw, &global); err != nil {
				return fmt.Errorf("decode global conversations: %w", err)
			}
			known := make(map[string]struct{}, len(first))
			for _, item := range first {
				known[item.ID] = struct{}{}
			}
			var newItems []conversation
			for _, item := range global {
				if _, ok := known[item.ID]; !ok {
					newItems = append(newItems, item)
				}
			}
			fmt.Printf("global conversations: PASS project_scoped_via_global=%d new_beyond_project_list=%d\n", len(global), len(newItems))
			if len(newItems) > 0 {
				combinedIDs := make([]string, 0, len(ids)+len(newItems))
				combinedIDs = append(combinedIDs, ids...)
				for _, item := range newItems {
					if len(combinedIDs) >= 20 {
						break
					}
					combinedIDs = append(combinedIDs, item.ID)
				}
				raw, err = call(client, request{ID: 14, Method: "conversationEvidence", ConversationIDs: combinedIDs})
				if err != nil {
					fmt.Printf("combined evidence (known + newly discovered): NOT VERIFIED (%v)\n", err)
				} else {
					var combinedEvidence []conversationEvidence
					if err := json.Unmarshal(raw, &combinedEvidence); err != nil {
						return fmt.Errorf("decode combined evidence: %w", err)
					}
					fmt.Printf("combined evidence: fields=%d sessions=%d (compare distinct_values/values here against the project-only evidence above; a field whose distinct_values rises only now is a discriminator candidate)\n",
						len(combinedEvidence), len(combinedIDs))
					for _, field := range combinedEvidence {
						fmt.Printf("combined evidence field: path=%s present=%d distinct_values=%d values=%s types=%s\n",
							field.Path, field.PresentCount, field.DistinctValueCount, strings.Join(field.Values, "|"), strings.Join(field.Types, ","))
					}
				}
			}
		}

		// Phase B: does our own script get to drive additional pages of this endpoint directly?
		// Recorded as evidence for the Recommendation, not used as the enumeration mechanism below.
		raw, err = call(client, request{ID: 99, Method: "globalConversationsPage", ProjectID: projectID,
			Params: map[string]any{"offset": 0, "limit": 28, "order": "updated", "is_archived": false, "is_starred": false}})
		if err != nil {
			fmt.Printf("self-initiated fetch of /backend-api/conversations: NOT AUTHORIZED (%v)\n", err)
		} else {
			var result globalConversationsPageResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return fmt.Errorf("decode global conversations page: %w", err)
			}
			fmt.Printf("self-initiated fetch of /backend-api/conversations: PASS raw_item_count=%d project_filtered_count=%d meta=%v\n",
				result.RawItemCount, len(result.Items), result.Meta)
		}

		all, pagination, coverage, pagesUsed, err := enumerateAllConversations(client, projectID, ids, 8, 20)
		if err != nil {
			fmt.Printf("global conversations complete enumeration: FAIL (%v)\n", err)
		} else {
			overall := "INCOMPLETE"
			if pagination.Status == "COMPLETE" && coverage.Status == "COMPLETE" {
				overall = "COMPLETE"
			}
			fmt.Printf("global conversations complete enumeration: pagination=%s (%s) coverage=%s (%s) overall=%s count=%d pages_used=%d\n",
				pagination.Status, pagination.Reason, coverage.Status, coverage.Reason, overall, len(all), pagesUsed)
		}

		raw, err = call(client, request{ID: 15, Method: "openURLProbe", ProjectID: projectID})
		if err != nil {
			fmt.Printf("open URL probe: NOT VERIFIED (%v)\n", err)
		} else {
			var probe openURLProbeResult
			if err := json.Unmarshal(raw, &probe); err != nil {
				return fmt.Errorf("decode open URL probe: %w", err)
			}
			printProbe := func(label string, r urlProbeResult) {
				fmt.Printf("open URL probe (%s): style=%s ready=%s login_prompt=%t href_matches_expected_path=%t redacted_href=%s redacted_expected=%s\n",
					label, r.Style, r.ReadyState, r.LoginPromptVisible, r.HrefMatchesExpectedPath, r.RedactedHref, r.RedactedExpectedPath)
			}
			printProbe("work-like, canonical /c/{id}", probe.WorkLikeCanonical)
			printProbe("work-like, project-scoped /g/{project}/c/{id}", probe.WorkLikeProjectScoped)
			printProbe("chat-like, canonical /c/{id}", probe.ChatLikeCanonical)
			printProbe("chat-like, project-scoped /g/{project}/c/{id}", probe.ChatLikeProjectScoped)
		}
	}

	if cfg.hold > 0 {
		fmt.Printf("holding helper for %s\n", cfg.hold)
		timer := time.NewTimer(cfg.hold)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if _, err := call(client, request{ID: 6, Method: "ping"}); err != nil {
			return fmt.Errorf("helper did not survive hold: %w", err)
		}
		raw, err := call(client, request{ID: 7, Method: "pageInfo"})
		if err != nil {
			return fmt.Errorf("page unavailable after hold: %w", err)
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode pageInfo after hold: %w", err)
		}
		fmt.Printf("helper sustained: PASS ready=%s origin=%s\n", page.ReadyState, origin(page.Href))
	}
	if err := terminate(cmd); err != nil {
		return err
	}
	return nil
}

func dialBridge(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("bridge unavailable after %s: %w", timeout, lastErr)
}

func call(conn net.Conn, req request) (json.RawMessage, error) {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send %s: %w", req.Method, err)
	}
	var res response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&res); err != nil {
		return nil, fmt.Errorf("receive %s: %w", req.Method, err)
	}
	if res.ID != req.ID {
		return nil, fmt.Errorf("response ID mismatch: got %d, want %d", res.ID, req.ID)
	}
	if !res.OK {
		return nil, errors.New(res.Error)
	}
	if len(res.Result) == 0 {
		return nil, errors.New("successful response has no result")
	}
	return res.Result, nil
}

func discoverConversations(conn net.Conn, id int, projectID string) ([]conversation, error) {
	raw, err := call(conn, request{ID: id, Method: "conversations", ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("discover conversations: %w", err)
	}
	var conversations []conversation
	if err := json.Unmarshal(raw, &conversations); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}
	for _, item := range conversations {
		if item.ID == "" || item.ProjectID != projectID {
			return nil, errors.New("conversation response is missing stable identity or Project association")
		}
	}
	return conversations, nil
}

func sameIDs(left, right []conversation) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, item := range left {
		seen[item.ID] = struct{}{}
	}
	for _, item := range right {
		if _, ok := seen[item.ID]; !ok {
			return false
		}
	}
	return true
}

func printDiscriminatorEvidence(conversations []conversation) {
	keys := make(map[string]struct{})
	values := make(map[string]struct{})
	relevantSchemaKeys := make(map[string]struct{})
	for _, item := range conversations {
		for key, value := range item.Discriminator {
			keys[key] = struct{}{}
			values[fmt.Sprintf("%s=%v", key, value)] = struct{}{}
		}
		for _, key := range item.AvailableKeys {
			lower := strings.ToLower(key)
			for _, marker := range []string{"work", "agent", "async", "type", "kind", "status", "mode"} {
				if strings.Contains(lower, marker) {
					relevantSchemaKeys[key] = struct{}{}
					break
				}
			}
		}
	}
	fmt.Printf("discriminator candidates: fields=%s distinct_values=%d relevant_schema_fields=%s\n",
		strings.Join(sortedSet(keys), ","), len(values), strings.Join(sortedSet(relevantSchemaKeys), ","))
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// enumerateAllConversations drives the only mechanism available to this bridge for reaching
// additional pages of the Project-inclusive /backend-api/conversations view: it cannot issue
// further authenticated requests itself (see the self-initiated-fetch probe in run(), which
// observes HTTP 401), so it repeatedly asks the real ChatGPT page to scroll its own conversation
// list and passively harvests whatever new captures that produces, tracked by CaptureID (a
// bridge-internal reference to one observed response — not a pagination position; see
// globalConversationsPageDiagnostic). Each newly observed capture is printed as sanitized wire
// evidence (series/offset/limit, hide_snorlax/is_archived/is_starred/unknown-parameter descriptor,
// raw/recognized item counts, a shortened raw identity digest — never conversation content or full
// IDs) before being folded into evaluateActiveListCompleteness, which does the actual
// pagination-identity, target-series-coverage, and exhaustion reasoning.
//
// knownConversationIDs (the project-scoped endpoint's already-discovered IDs) is passed through to
// the bridge on every capture-items fetch, so it can flag a known Project conversation whose
// association no longer resolves to the configured Project — see mergeProjectPages's
// KnownIDsMismatched handling.
func enumerateAllConversations(client net.Conn, projectID string, knownConversationIDs []string, maxScrollAttempts, maxPages int) (conversations []conversation, pagination completenessResult, coverage completenessResult, pagesUsed int, err error) {
	seenCaptures := make(map[int]bool)
	var pages []conversationPage
	nextRequestID := 300

	fetchNewPages := func() error {
		nextRequestID++
		raw, err := call(client, request{ID: nextRequestID, Method: "globalConversationsPages"})
		if err != nil {
			return fmt.Errorf("list global conversations pages: %w", err)
		}
		var diagnostics []globalConversationsPageDiagnostic
		if err := json.Unmarshal(raw, &diagnostics); err != nil {
			return fmt.Errorf("decode global conversations pages: %w", err)
		}
		for _, d := range diagnostics {
			if seenCaptures[d.CaptureID] {
				continue
			}
			seenCaptures[d.CaptureID] = true
			offsetStr, limitStr := "?", "?"
			if d.Offset != nil {
				offsetStr = fmt.Sprintf("%d", *d.Offset)
			}
			if d.Limit != nil {
				limitStr = fmt.Sprintf("%d", *d.Limit)
			}
			seriesDisplay := "?"
			if d.SeriesKey != nil {
				seriesDisplay = shortDigest(*d.SeriesKey)
			}
			queryParts := make([]string, 0, len(d.Query))
			for _, q := range d.Query {
				queryParts = append(queryParts, q.Key+"="+q.Value)
			}
			isArchivedStr, isStarredStr, orderStr := "?", "?", "?"
			if d.IsArchived != nil {
				isArchivedStr = fmt.Sprintf("%t", *d.IsArchived)
			}
			if d.IsStarred != nil {
				isStarredStr = fmt.Sprintf("%t", *d.IsStarred)
			}
			if d.Order != nil {
				orderStr = *d.Order
			}
			fmt.Printf("global conversations wire capture #%d: hide_snorlax=%t is_archived=%s is_starred=%s order=%s unknown_parameters=%t series=%s query=[%s] offset=%s limit=%s recognized_collection=%t raw_item_count=%d recognized_id_count=%d digest=%s meta=%v\n",
				d.CaptureID, d.HideSnorlax, isArchivedStr, isStarredStr, orderStr, d.HasUnknownParameters, seriesDisplay, strings.Join(queryParts, ","), offsetStr, limitStr, d.RecognizedCollection, d.RawItemCount, d.RecognizedIDCount, shortDigest(d.RawIdentityDigest), d.Meta)
			if d.HideSnorlax {
				continue // excludes Project conversations by construction; recorded above for evidence only
			}
			if d.SeriesKey == nil {
				return fmt.Errorf("capture %d has no usable pagination series identity", d.CaptureID)
			}
			if !d.RecognizedCollection {
				return fmt.Errorf("capture %d has an unrecognized item-collection shape (must not be treated as an empty page)", d.CaptureID)
			}
			if d.Offset == nil || d.Limit == nil || *d.Limit <= 0 {
				return fmt.Errorf("capture %d is missing usable offset/limit pagination metadata", d.CaptureID)
			}
			nextRequestID++
			itemsRaw, err := call(client, request{
				ID: nextRequestID, Method: "globalConversationsCaptureItems", ProjectID: projectID,
				CaptureID: d.CaptureID, KnownConversationIDs: knownConversationIDs,
			})
			if err != nil {
				return fmt.Errorf("fetch items for capture %d: %w", d.CaptureID, err)
			}
			var result globalConversationsCaptureItemsResult
			if err := json.Unmarshal(itemsRaw, &result); err != nil {
				return fmt.Errorf("decode items for capture %d: %w", d.CaptureID, err)
			}
			pages = append(pages, conversationPage{
				SeriesKey:                    *d.SeriesKey,
				IsArchived:                   d.IsArchived,
				IsStarred:                    d.IsStarred,
				HasUnknownParameters:         d.HasUnknownParameters,
				Offset:                       *d.Offset,
				Limit:                        *d.Limit,
				RecognizedCollection:         d.RecognizedCollection,
				RawItemCount:                 d.RawItemCount,
				RecognizedIDCount:            d.RecognizedIDCount,
				RawIdentityDigest:            d.RawIdentityDigest,
				KnownIDsMismatched:           result.KnownIDsMismatched,
				UnrecognizedAssociationCount: result.UnrecognizedAssociationCount,
				Items:                        result.Items,
			})
		}
		return nil
	}

	if err := fetchNewPages(); err != nil {
		return nil, completenessResult{}, completenessResult{}, 0, err
	}
	for attempt := 0; attempt < maxScrollAttempts; attempt++ {
		conversations, pagination, coverage, err = evaluateActiveListCompleteness(pages, maxPages)
		if err != nil {
			return nil, completenessResult{}, completenessResult{}, len(pages), err
		}
		if pagination.Status == "COMPLETE" {
			return conversations, pagination, coverage, len(pages), nil
		}
		nextRequestID++
		scrollRaw, err := call(client, request{ID: nextRequestID, Method: "simulateSidebarScroll"})
		if err != nil {
			return conversations, pagination, coverage, len(pages), fmt.Errorf("simulate sidebar scroll: %w", err)
		}
		var scroll sidebarScrollResult
		if err := json.Unmarshal(scrollRaw, &scroll); err != nil {
			return conversations, pagination, coverage, len(pages), fmt.Errorf("decode sidebar scroll result: %w", err)
		}
		fmt.Printf("sidebar scroll simulation (attempt %d): triggered=%t reason=%q containers=%d links_before=%d links_after=%d\n",
			attempt, scroll.Triggered, scroll.Reason, scroll.ContainerCount, scroll.ConversationLinkCountBefore, scroll.ConversationLinkCountAfter)
		time.Sleep(1500 * time.Millisecond)
		if err := fetchNewPages(); err != nil {
			return conversations, pagination, coverage, len(pages), err
		}
	}
	conversations, pagination, coverage, err = evaluateActiveListCompleteness(pages, maxPages)
	if err != nil {
		return nil, completenessResult{}, completenessResult{}, len(pages), err
	}
	return conversations, pagination, coverage, len(pages), nil
}

func origin(rawURL string) string {
	if strings.HasPrefix(rawURL, "https://chatgpt.com") {
		return "https://chatgpt.com"
	}
	return "unexpected"
}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop terminal-browser: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case <-time.After(5 * time.Second):
		return errors.New("terminal-browser did not stop after SIGTERM")
	case err := <-wait:
		if err == nil || strings.Contains(err.Error(), "signal: terminated") {
			return nil
		}
		return fmt.Errorf("terminal-browser exited: %w", err)
	}
}
