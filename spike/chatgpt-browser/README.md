# ChatGPT browser-backed integration spike

## Purpose

This spike collects evidence for [Issue #7](https://github.com/tingtt/agentsctl/issues/7). It tests whether `terminal-browser` can own ChatGPT authentication and UI while a small local bridge exposes only Project and conversation metadata to Go.

This is not the ChatGPT provider planned by Issue #6. It does not change the production provider interface, shared session model, capabilities, Agent View, composer, dispatch, configuration schema, or existing providers.

The observed ChatGPT `/backend-api/...` endpoints are **undocumented and unstable**. They are validation targets, not supported APIs.

## Architecture

```text
Go spike process
  ├─ owns and drains a pseudo-PTY
  ├─ starts terminal-browser with a persistent partition
  └─ speaks newline-delimited JSON over a mode-0600 Unix socket
                       │
                       ▼
terminal-browser main script
  └─ validates methods and routes requests over Electron IPC
                       │
                       ▼
isolated preload in chatgpt.com
  ├─ performs same-origin browser fetches
  └─ returns allowlisted, sanitized metadata only
```

The bridge protocol has these browser methods:

- `pageInfo`: URL, title presence, document readiness, and a non-authoritative login UI signal.
- `projects`: sanitized Project ID and name (no URL — see the Phase 7/Phase 5-follow-up note on Project URLs in Known limitations).
- `conversations`: sanitized identity, title, times, archived state, Project association, selected discriminator candidates, response keys, and Open URL candidates, from the project-scoped endpoint.
- `globalConversations`: the same shape, but sourced from the global (unscoped) conversations endpoint and filtered client-side to the requested Project — needed because the project-scoped endpoint alone under-reports (see Phase 5). Captures whichever `/backend-api/conversations` response arrives first; the Phase 5 pagination follow-up found this can non-deterministically be a `hide_snorlax=true` response that structurally excludes every Project conversation (see below), so this method's count alone is not reliable evidence of completeness.
- `conversationEvidence`: cross-conversation field/cardinality comparison for up to 20 IDs at once, used to hunt for a Chat/Work discriminator (Phase 6) without exposing conversation content, titles, or per-conversation identity mapping — allowlisted structural values (status/type/kind/mode/origin-style labels) are ID-redacted before being returned.
- `tasks`: sanitized evidence from the observed, undocumented `/backend-api/tasks` endpoint — investigated as a candidate Work-discovery path and rejected (see Phase 5); reports only counts and structural status-field names, never task content.
- `openURLProbe`: internally selects one conversation showing the Chat/Work discriminator candidate and one plain conversation (the chosen IDs never cross the socket), navigates each to both `/c/{id}` and `/g/{project_id}/c/{id}`, and reports readiness/login-redirect state plus an ID-redacted URL comparison (Phase 7).
- `globalConversationsPages`: sanitized, per-capture wire diagnostics for every `/backend-api/conversations` response seen so far, addressed by an immutable `CaptureID` (a bridge-internal reference to one observed response — never a pagination position). Each entry carries the actual pagination identity as `seriesKey` — a SHA-256 digest of the RAW, unredacted query with `offset` removed, sorted and JSON-encoded for unambiguous normalization — plus `offset`/`limit` from the response's own metadata (`null` if unusable rather than assumed to be 0). A separate, human-readable `query` array (redacted the same way as before) is included for diagnostics only and is never used to decide pagination identity, since two different opaque values of the same length would otherwise redact to the same placeholder and collide. Also carries `hideSnorlax`/`isArchived`/`isStarred`/`order` — each a recognized value (e.g. `"true"`, `"false"`, `"updated"`), or the literal `"absent"` (key not present) or `"unknown"` (present with a value outside the recognized set); never a bare boolean that could silently fold "unrecognized" into "false" — `hasUnknownParameters`, `recognizedCollection` (false if the response's item-collection shape itself couldn't be located — never treated as "0 items"), response top-level keys, a scalar-only `meta`, raw and recognized item counts, and a `rawIdentityDigest` fingerprinting the raw page's own conversation identity (not just this Project's subset). Added for the Phase 5 pagination follow-up (2026-09-11), reworked four times since for pagination-identity, schema-drift, coverage, and classifier correctness; never exposes conversation content, IDs, or raw query values.
- `globalConversationsCaptureItems`: sanitized, Project-filtered conversation items for one already-observed capture (addressed by `CaptureID`, not array position — an evicted or unknown ID is a distinct, explicit error). Also returns two independent schema-drift signals over the capture's raw items: cross-validation of a caller-supplied allowlist of already-known Project conversation IDs against their raw association data (`knownIDsMismatched`), and a universal per-item check that every raw item exposes a recognizable `gizmo_id`/`project_id` own property at all — present but `null` for an ordinary non-Project chat, absent only when the schema has actually drifted (`unrecognizedAssociationCount`; see Known limitations for what this can and cannot prove). Added for the same follow-up.
- `globalConversationsPage`: issues its own same-origin `fetch` to `/backend-api/conversations` with caller-supplied query parameters, rather than waiting for a passive observation. Added to test whether the bridge can self-drive pagination; empirically returns HTTP 401 (see Phase 5 follow-up) — kept only as a documented negative probe, not part of the enumeration path.
- `simulateSidebarScroll`: dispatches synthetic `scroll` events at every scrollable ancestor of the sidebar's `/c/{id}` anchors, to test whether official ChatGPT UI issues further `/backend-api/conversations` requests as a real user would trigger by scrolling. Added for the same follow-up.
- `pins`: passively captures and sanitizes the observed, undocumented `/backend-api/pins` endpoint — supporting evidence for the Phase 5 `is_starred` coverage experiment (see the Phase 5 fifth-pass addendum). Reports only a raw top-level item count, a recognized-conversation-ID count, and the recognized conversation IDs themselves (never titles or other content), plus a capture generation counter so a caller can tell a fresh observation from a stale one.

`ping` terminates in the main script and proves the Go-to-socket portion independently of page readiness. Unknown methods, malformed IDs, non-JSON responses, non-2xx responses, and unrecognized response shapes fail closed.

## How to run

Requirements:

- macOS or another Unix host supported by `github.com/creack/pty`.
- `terminal-browser` on `PATH`.
- A terminal with kitty graphics protocol support for manual login and UI experiments.

From this directory:

```bash
go test .
go run . -hold 35s
go run . -project-name agentsctl
go run . -project-name agentsctl -pin-experiment
```

The last form runs the interactive Phase 5 `is_starred` coverage experiment (see the Phase 5 fifth-pass addendum, and the ninth/tenth-pass addenda for the fresh-process fallback this now uses): it prompts the operator, via stdout, to pin and later unpin one conversation of their own choosing directly in the real ChatGPT UI, and reports target-series membership evidence identified from the `/backend-api/pins` before/after delta. It requires an interactive terminal (it blocks on stdin between phases) and a human performing the actual pin/star actions — this program never issues that mutation itself. Between phases, it restarts the `terminal-browser` process itself (same persistent partition) rather than asking the operator to reload — do not manually reload when prompted; just perform the pin/unpin action and press Enter.

The Project name is used only for the one-time lookup in this spike. The proposed production configuration remains ID-based:

```toml
[chatgpt]
project_id = "g-p-..."
```

The harness intentionally does not print Project names, Project IDs, conversation titles, or conversation IDs. Use `-project-id` when the ID is already known and should not be printed.

Lifecycle probes:

```bash
go run . -hold 15s -close-pty-after 5s
go run . -terminal-browser /definitely/missing/terminal-browser
```

## Manual login

Only the human user should perform login, MFA, or OAuth steps:

```bash
terminal-browser open https://chatgpt.com \
  --partition=agentsctl-chatgpt \
  --no-merge
```

After login, quit the browser normally and run the same command again. Authentication persistence is proven only if an authenticated, non-sensitive browser request succeeds after that restart. DOM absence of a login button is not sufficient evidence.

Do not dump cookies, inspect browser credential databases, extract bearer or refresh tokens, or reuse `~/.codex/auth.json`.

## Experiments

Results below were recorded 2026-09-09 through 2026-09-11 on macOS 14.4 / Darwin 23.4.0 arm64, Go 1.26.0, and `terminal-browser` v0.8.0. The current upstream release was v0.8.1; upstream `main` was `b16b8574a026ba0ef451e7e377e12b5747c47706`. Phases 0–3 ran in a sandboxed harness terminal without kitty graphics support; Phases 1 and 4–7 were re-run by the repository owner from their own terminal, against their real ChatGPT account and a real Project, after one manual login and after creating one ChatGPT Work/Agent session for Phase 6/7 testing.

### Phase 0: baseline

**Hypothesis:** The repository and installed runtime can support an isolated Go spike.

**Method:** Inspected Git state, tool versions, help output, dependency declarations, terminal capability, and current upstream source. Ran the existing test suite without mutation.

**Observed:** The `main` worktree was clean. Go 1.26.0 and `github.com/creack/pty` v1.1.24 were available. Installed `terminal-browser` was v0.8.0 at `/Users/taku_ting/.local/bin/terminal-browser`, one release behind v0.8.1. The current Codex terminal failed the kitty graphics check. The unprivileged full suite was sandbox-blocked on Go cache and Unix socket access; this was an environment failure, not a baseline test failure.

**Result: CONDITIONAL**

**Implication:** Background tests can run by explicitly skipping the graphics check, but visible UI tests require a kitty-capable terminal. Version drift must be included in compatibility decisions.

### Phase 1: persistent authentication

**Hypothesis:** A dedicated partition preserves a safely reusable ChatGPT login across browser restarts.

**Method:** Started `agentsctl-chatgpt`, stopped its owner, restarted the same partition, and requested the Project sidebar inside the browser context.

**Observed (run 1, sandboxed session, no kitty terminal):** The partition was logged out: the browser request returned HTTP 401 after restart. The attempted foreground login browser could not render because the terminal lacked kitty graphics support. No credential fallback was attempted.

**Observed (run 2, human-operated terminal, after manual login outside this harness):** `go run . -hold 35s` against the same `agentsctl-chatgpt` partition, in a fresh helper process, reported `page: ... login_prompt=false account_control=true` immediately on load and again after a 35s hold with `ready=complete`. Both are non-sensitive DOM signals (absence of a "Log in"/"Sign up" control and presence of an account/profile control), not a cookie or token check.

**Observed (run 3, same partition, immediately following):** `go run . -project-name agentsctl -hold 10s` performed an authenticated same-origin `fetch` of the sidebar and Project-conversations endpoints and got real data back: `projects: PASS count=18`, `conversations: PASS count=5 repeated_ids_stable=true`. This is a genuine authenticated-request proof, not a DOM signal — a stale or absent session would have surfaced as HTTP 401, exactly as it did in run 1.

**Result: PASS**

**Implication:** Login performed once by the human in `agentsctl-chatgpt` survived a full helper stop/restart cycle and produced two successful authenticated fetches against real endpoints. Persistent authentication for this partition is proven for this session; long-term persistence (across days, token refresh boundaries) is not covered by this single restart.

```text
persistent login: PASS (manual login performed once by the human outside this harness)
restart persistence: PASS — confirmed by authenticated fetch (projects count=18, conversations count=5) after a stop/restart cycle, not just DOM signal
manual login required: yes — performed once
notes: run 1 (sandboxed, no kitty terminal) was logged out (HTTP 401); runs 2-3 (human terminal, same partition, after manual login) showed authenticated DOM state and successful authenticated fetches after a fresh restart
```

### Phase 2: background helper feasibility

**Hypothesis:** A Go-owned pseudo-PTY can keep stock `terminal-browser` alive without drawing its output.

**Method:** Started the browser with `TERMINAL_BROWSER_SKIP_GRAPHICS_CHECK=1`, continuously copied PTY output to `io.Discard`, held it for 35 seconds, then repeated bridge and page probes. Separately closed the PTY master after 5 seconds and inspected the browser registry after owner exit.

**Observed:** Browser and bridge remained alive for 35 seconds; the page reached `readyState=complete`. Closing the PTY caused the bridge to fail with `broken pipe`. Owner exit removed the browser from `terminal-browser ls --all --json`. A bridge socket client could disconnect and reconnect without ending the browser, but the terminal-browser client/PTY could not.

**Result: CONDITIONAL — B. PoC workaround only**

**Implication:** The pseudo-PTY is an effective experiment harness, not a production service lifecycle. A supported no-render/service mode or equivalent upstream lifecycle contract is needed.

### Phase 3: browser bridge

**Hypothesis:** A request can cross Go, a local socket, Electron main IPC, preload, the ChatGPT page, and return without exposing browser credentials.

**Method:** Sent `ping` and `pageInfo` through the mode-0600 socket. Closed and reconnected the Go socket client. Held the helper for 35 seconds and repeated the probes.

**Observed:** Initial and post-hold round trips passed. The page reported the expected origin and eventually reached `readyState=complete`. No cookie, authorization header, token, storage value, or raw API payload crossed the bridge.

**Result: PASS**

**Implication:** `--main-script` plus `--preload` is viable as the narrow transport. Its lifecycle still inherits the Phase 2 limitation.

### Phase 4: Project discovery

**Hypothesis:** The observed sidebar endpoint can resolve the configured Project by stable `g-p-...` ID.

**Method (run 1, sandboxed, logged out):** Called observed, undocumented `GET /backend-api/gizmos/snorlax/sidebar` inside the browser and allowed only recognized Project metadata through the sanitizer.

**Observed (run 1):** HTTP 401 in the logged-out partition. No response schema or Project metadata was accepted.

**Method (run 2, human terminal, authenticated partition):** `go run . -project-name agentsctl -hold 10s`. The harness fetched the sidebar payload, sanitized it to `{id, name, url}` triples, and resolved the single Project matching the given name to its ID without printing either.

**Observed (run 2):** `projects: PASS count=18 (names and IDs not logged)`. Name-to-ID resolution succeeded unambiguously (the harness errors out on 0 or >1 matches, and did neither). `capture: ... projects=true` confirmed the underlying debugger capture matched the exact endpoint path.

**Result: PASS** (for existence, authenticated fetch, and unambiguous name→ID resolution). **NOT VERIFIED** for rename-survives-ID and duplicate-name-disambiguation specifically, since those require mutating an existing Project or having two Projects share a name — neither was attempted, consistent with the no-destructive-ops constraint.

**Implication:** Stable ID resolution from an authenticated context is proven. Rename/duplicate-name robustness remains a documented, untested assumption rather than a proven property.

### Phase 5: session discovery

**Hypothesis:** The observed Project conversations endpoint yields a reproducible session list.

**Method (round 1):** Called observed, undocumented `GET /backend-api/gizmos/{project_id}/conversations` twice in sequence for the resolved Project and compared the returned conversation IDs.

**Observed (round 1):** `conversations: PASS count=5 repeated_ids_stable=true (titles and IDs not logged)`. Both calls returned the same 5 conversation identities (`sameIDs` requires exact set equality, ignoring order); every item carried a non-empty ID and the requested Project association, or the harness would have failed closed.

**Method (round 2, after a human created one ChatGPT Work/Agent session in the same Project):** Reran the same command. The project-scoped endpoint still returned exactly 5, unchanged. To check whether that endpoint was structurally incomplete rather than just missing the new item, the harness was extended to also capture the observed, undocumented global (unscoped) `GET /backend-api/conversations` and locally filter it to entries whose `gizmo_id`/`project_id` matched the resolved Project — without ever sending the raw payload or the filtered IDs across the socket.

**Observed (round 2):** `global conversations: PASS project_scoped_via_global=17 new_beyond_project_list=13`. The global, Project-filtered view found 17 conversations associated with the Project — 13 more than the project-scoped endpoint returned. The project-scoped `/backend-api/gizmos/{project_id}/conversations` endpoint is not a complete session list; it appears to return only a bounded/recent subset. A separate, undocumented `GET /backend-api/tasks` was also captured as a candidate Work-discovery endpoint and rejected: `tasks: PASS conversation_id_like=50 overlap_with_project_conversations=0 status_fields=status distinct_status_values=3` — 50 items, none of which overlap this Project's known conversations, consistent with `/backend-api/tasks` being ChatGPT's separate scheduled-automation "Tasks" feature, not Agent/Work-mode conversations.

**Result: PASS, with a corrected method.** Reproducible session discovery requires the global `/backend-api/conversations` endpoint filtered client-side by Project association, not the project-scoped endpoint alone. `/backend-api/tasks` is a rejected lead, not a discovery path.

**Implication:** `chatgpt:<conversation_id>` is a reproducible identity (stable across the two project-scoped reads, and the newly-discovered items were also stable IDs on inspection). The real blocker this phase surfaces is a discovery-completeness gap, not an identity-stability gap: a production adapter built only against `/backend-api/gizmos/{project_id}/conversations` would silently miss most of a Project's sessions, Work included.

#### Phase 5 addendum: pagination and completeness follow-up (2026-09-11)

The round-2 result above established that the global endpoint out-reports the project-scoped one (17 vs. 5), but never established that the global endpoint's own single captured response was itself complete — `/backend-api/conversations` could in principle paginate, and a single-response capture would silently under-report just like the project-scoped endpoint did. This follow-up investigated that specific gap.

**Phase A (inspect the existing implementation, code-reading only):** `capturedGlobalConversations` in `bridge/main.js` is a single variable, overwritten by whichever `/backend-api/conversations` response the passive CDP capture sees first; `globalConversationsFrom` filters that one payload's nested objects by Project ID. There was no cursor, offset, or "fetch until exhausted" logic anywhere in the path — the round-2 PASS was a property of one response, not of a verified-complete one. This matched the task's warning not to judge API completeness from reading code alone, so it was treated as an open question rather than a finding.

**Phase B (observe real wire behavior, live account, `-project-name agentsctl`):** Extending the CDP capture to log query keys/values (opaque values length-redacted) and response top-level keys per observed request revealed:

- The endpoint takes `offset`, `limit` (28 in every observation), `order` (`updated`), `is_archived`, `is_starred`, and a previously-undocumented `hide_snorlax` flag; responses carry `items`, `limit`, `offset`, and `total`.
- **`hide_snorlax=true` excludes every Project/gizmo-associated conversation from `items`.** Across every live observation, a `hide_snorlax=true` page's Project-filtered item count was exactly 0, regardless of how many raw items it returned; only `hide_snorlax` false-or-absent pages ever contained this Project's conversations. `gizmo_type: "snorlax"` was already known (Phase 6) as the internal label for Project/gizmo conversations, so this is very likely what the flag is named for. **This means the original Phase 5 round-2 result was not deterministically correct: the single-shot capture could just as easily have landed on a `hide_snorlax=true` response and reported 0 Project conversations from the "corrected" method, with no error to signal that.** `globalConversations`'s doc comment above now reflects this.
- `total` is not trustworthy as an exhaustion signal: at the same point in a session it read identically (29) for both a `hide_snorlax=true` and a `hide_snorlax=false` page despite their very different item-level filtering, and after a scroll-triggered second page of the `hide_snorlax=true` series it jumped 29 → 57 with no plausible 28 new conversations having been created — evidence, not assumption, of an approximate/filter-unaware counter, not a stable upper bound.
- Simulating sidebar scroll (dispatching synthetic `scroll` events at the sidebar's scrollable containers) did make the real ChatGPT UI issue a further request with `offset` advanced from 0 to 28 — proof that official ChatGPT Web does paginate this endpoint via further requests as a user scrolls — but only ever on the `hide_snorlax=true` series. The `hide_snorlax` false/absent series (the one that includes this Project's conversations) was never observed advancing past `offset=0` in any run, including runs where the scroll simulation successfully advanced the other series.
- A same-origin `fetch` issued by the bridge's own script directly (not observed passively) was rejected with **HTTP 401** in every attempt. Cookies alone (`credentials: "include"`) are not sufficient for this endpoint from an arbitrary script context; whatever the real ChatGPT client attaches beyond cookies is not something this bridge has, or should try to obtain (extracting it would cross the credential boundary). This also means the bridge's dead-code direct-`fetch` branches for `projects`/`tasks`/`conversations` (never reached, because `main.js` always intercepts those methods and serves passively-captured data first) were themselves untested and would likely fail the same way if ever reached.

**Phase C (pagination contract classification):** **C. offset/page pagination** for the wire mechanism itself — `offset`/`limit` plus an `items` array is an explicit, non-cursor contract, directly observed advancing under real UI action. This is *not* classified as A (single response complete) merely because a response contained N items, per the task's explicit warning.

**Phase D (attempt complete enumeration):** `main.go` gained `mergeProjectPages` — a pure function (no live browser needed, fully unit-tested in `main_test.go`) that dedupes conversations by ID across observed pages and decides exhaustion only from a page whose raw item count is strictly less than its requested `limit` (never from `total`, given the instability above), and `enumerateAllConversations`, which drives the only mechanism this bridge actually has for reaching further pages — repeatedly asking the real page to scroll and passively harvesting whatever that produces — since self-issued fetches 401. It fails closed (returns an error, not a partial success) on: a `hide_snorlax`-excluding page reaching the merge step, a non-positive `limit`, a conversation missing a stable ID, the same page sequence reporting different conversations on re-observation, and more pages than a defensive bound (20).

**Observed (live, three separate runs on 2026-09-11):** In every run, `global conversations complete enumeration: INCOMPLETE (exhaustion not observed within bound) count_so_far=16 pages_used=N` — the loop correctly refused to report completion, because the only page series that includes this Project's conversations was never observed advancing past `offset=0`, and a full (28-of-28) first page cannot by itself prove no second page exists.

**Result: CONDITIONAL, method-corrected again.** The pagination *mechanism* is now positively identified (offset/limit, real UI-driven advancement proven for one query variant), and the *filter* that determines Project-conversation inclusion is now known (`hide_snorlax`) where the original round-2 method didn't check it at all. But this Project's current size (≤ 28, one page) means true multi-page behavior of the Project-inclusive query was never exercised, and there is no way for this bridge to force it: self-fetch is unauthorized, and the real UI's own scroll-driven pagination — the only mechanism proven to advance this endpoint at all — was only ever observed advancing the *other*, Project-excluding query series.

**Implication:** `count_so_far=16` (deduped across repeated observations of the same first page) is corroborated by the project-scoped endpoint's historical count (5, a known-incomplete subset) and the original round-2 count (17, one snapshot earlier, normal live-account drift), which is reasonable completeness evidence *for a Project this size*, but it is not proof the mechanism scales to a Project whose conversations exceed one page. A production adapter cannot assume the single-page-suffices result observed here generalizes; see the updated Recommendation.

**Post-review hardening (2026-09-11, same day):** a review of this PoC found the first implementation's pagination identity was wrong, not just incomplete: it used the capture arrival-order index as if it were a page/offset position, so a history-buffer eviction could silently reinterpret an old index as a different page, and it never checked that two captures actually belonged to the same query filter (e.g. `is_archived`) before treating them as continuations of one series. It also compared re-observations of "the same page" using the already-Project-filtered item list rather than the raw page, which could mask a raw-page change that happened not to affect the filtered subset. This was corrected: every capture now gets an immutable CaptureID (a bridge-internal reference only, never a pagination position); the real pagination identity is an explicit `SeriesKey` (the query with `offset` removed) plus `Offset`, both taken from the response's own metadata; exhaustion requires a *contiguous* `offset=0, limit, 2×limit, ...` chain ending in a short page, not merely the highest-numbered capture seen; re-observation consistency is checked against a raw-page identity digest (all raw item IDs, not just this Project's); and a page is now rejected as schema drift if any raw item's ID can't be recognized, or if a conversation ID already known from the project-scoped endpoint no longer resolves to the configured Project. Re-running against the same account afterward reproduced the same result — `INCOMPLETE`, count 16 — confirming the hardening changed *how carefully* the PoC checks its own work, not the underlying Phase 5 conclusion.

**Second post-review hardening pass (2026-09-11, same day):** a further review found three more ways the pagination PoC could still misjudge completeness. First, an unrecognized item-collection shape (e.g. a future schema renaming `items`) fell through as `rawItemCount=0`, indistinguishable from a legitimate empty final page — fixed with an explicit `recognizedCollection` flag that fails closed when false, checked before any short-page/exhaustion reasoning runs. Second, `SeriesKey` was being built from the already-redacted diagnostic query, where two different opaque values of the same length both display as `<redacted:Nch>` and could therefore collide into one (wrong) series — fixed by computing it instead as a SHA-256 digest of the raw, unredacted, offset-excluded, sorted query, so no raw value crosses the bridge but no collision is possible either. Third, the association-schema-drift check was previously limited to conversation IDs already known from the project-scoped endpoint, which could miss drift on a page containing only conversations the harness had never seen before; live evidence (multiple captures, 28 raw items each) showed every conversation item — Project-associated or not — always carries `gizmo_id` or `project_id` as an own property (present but `null` for an ordinary non-Project chat), so this was safely strengthened into a universal per-item check, kept alongside (not instead of) the known-ID cross-check. All three are unit-tested in `main_test.go`. Live re-verification (three runs) reproduced the same `INCOMPLETE`, count 16 result — Phase 5 remains CONDITIONAL.

**Third post-review hardening pass — pagination completeness vs. coverage completeness (2026-09-11, same day):** a further review found that even a fully pagination-exhausted `SeriesKey` proves nothing about *which* universe of sessions it represents — `is_starred=true` observed to be short and complete only proves "all starred conversations are enumerated," not "all Project conversations are." `SeriesKey` is an opaque identity digest by design (see above); it was never meant to carry meaning, so completeness judgments built directly on it were conflating "this page-series is exhausted" with "this is the page-series agentsctl needs."

This is now two explicit, separately-reported axes: **Pagination** (is the required series' offset chain exhausted) and **Coverage** (is the required-series *definition* itself trustworthy as the full intended universe). The intended universe was determined from the existing production code, not guessed: `session.Provider.List(ctx, archived bool)` already exists and `Controller.Load` always calls it with `archived=false` — "the normal List" is the active, non-archived session set, and archived is production's own separate, explicit dimension (there is no "starred" concept anywhere in agentsctl). Each capture now also carries a `seriesDescriptor` — allowlisted, safe semantic metadata (`isArchived`, `isStarred`, `order`, `hasUnknownParameters`) — kept deliberately separate from `SeriesKey`: the descriptor is used only to classify which universe a series belongs to, never as pagination identity, and any query parameter this bridge doesn't recognize sets `hasUnknownParameters`, excluding that series from being trusted as a known target regardless of how its other fields look.

The one required series for "active Project sessions" — `hide_snorlax=false`, `is_archived=false`, `is_starred=false`, no unrecognized parameter — matches exactly what the real ChatGPT UI's default view was observed sending every time. An irrelevant series (e.g. a hypothetical `is_archived=true` observation) is now excluded *before* it ever reaches schema/pagination validation, so it can never block or corrupt completeness of the series that actually matters — unit-tested directly, including the case where the irrelevant series is itself malformed.

One dimension could not be confirmed or ruled out: whether `is_starred=true` conversations sit in a separate bucket excluded from the default view. No real UI action in this account ever requested `is_starred=true` to test directly. Indirect evidence — a separate `/backend-api/pins` endpoint, observed independently in real traffic — suggests pin/star state is handled apart from list membership, but this was not directly tested. Coverage is therefore reported as **UNKNOWN**, not COMPLETE, even in every live run where Pagination reached exhaustion for the (as it happens, still-incomplete) target series. Overall completeness requires both axes COMPLETE; live re-verification (three runs) reproduced `pagination=INCOMPLETE coverage=UNKNOWN overall=INCOMPLETE count=16` every time — Phase 5 remains CONDITIONAL, and its remaining gap is now stated precisely as two separate open questions rather than one.

**Fourth post-review hardening pass — classifier hardening and live ingestion (2026-09-11, same day):** the third pass's claim that "an irrelevant series is excluded before it ever reaches schema/pagination validation" was only true of the pure `evaluateTargetPagination` function — the live-ingestion path (`enumerateAllConversations`) still ran collection/offset/limit validation and fetched/sanitized items for *every* non-`hide_snorlax` capture before any coverage-layer filtering happened, so a malformed but genuinely irrelevant capture (e.g. `is_archived=true` with an unrecognized collection shape) could fail the whole enumeration — a false FAIL, the mirror image of the false COMPLETE this hardening series has been closing off. Fixed by classifying every capture (`classifyForActiveList`) immediately after computing its descriptor and *before* any of that validation: **MATCH** proceeds to the existing pipeline; **DEFINITELY_IRRELEVANT** is recorded as evidence and skipped entirely, with no collection/offset/limit check and no item fetch; **UNKNOWN** is also skipped from pagination, but is never treated as harmless — it's collected and composed into `Coverage.Reason`, so an unclassifiable capture can never be silently ignored the way "irrelevant" is allowed to be.

This pass also closed two related gaps. First, `hide_snorlax`/`is_archived`/`is_starred`/`order` previously fell back to a plain `value == "true" ? true : false`-style boolean, silently treating any unrecognized value (a typo, a future new mode) the same as the confirmed "false" case; each is now one of a small recognized-value set, or the literal string `"absent"` (key not present) or `"unknown"` (present but unrecognized) — never coerced into a recognized value's meaning. `hide_snorlax`'s `"false"` and `"absent"` states are deliberately still treated alike (real evidence directly supports it — see above); `is_archived`/`is_starred`/`order` are not given the same benefit, since their `"absent"` state has never been directly observed on the real target series. Second, `order=updated` — previously tracked but not required — is now part of `activeListRequiredSeries` (`RequireOrder`): a different or absent order value no longer silently matches the target.

Live re-verification (three runs) reproduced the identical `pagination=INCOMPLETE coverage=UNKNOWN overall=INCOMPLETE count=16` result, with every real capture correctly classified `MATCH` (`hide_snorlax=absent is_archived=false is_starred=false order=updated`) — the classifier hardening changed how carefully captures are screened before validation, not the underlying Phase 5 conclusion.

**Fifth pass — is_starred coverage experiment tooling (2026-09-11, same day):** the classifier hardening pass above left one deliberate gap open: `activeListCoverageCaveat` states that whether `is_starred=false` excludes starred/pinned conversations was never directly tested, only inferred from indirect evidence (a separate `/backend-api/pins` endpoint observed in real traffic). This pass adds the spike tooling needed to test it directly, per the task that requested it: pin/star one active Project conversation via the real ChatGPT UI (a human-performed, reversible metadata mutation — this spike never issues that mutation itself, consistent with the security/mutation boundary above) and observe whether it disappears from the default `is_starred=false` target series.

A pre-selection design problem had to be solved first: the security boundary forbids this program from ever printing a conversation's title or real ID, but the operator needs to know *which* conversation to pin, and the program needs to know *which one was affected* to check its membership before and after. Asking the operator to identify a conversation by an ID or title this program prints would violate that boundary from the wrong direction (this program would have to display it first). The chosen design avoids identifying a conversation at all: the operator pins/unpins **any one** conversation of their own choosing directly in their own authenticated ChatGPT UI (which already shows them its title — this program never needs to), and the *affected* conversation is identified purely from the set difference between two enumerations of the active target series, reported only as a `conversationFingerprint` (SHA-256, truncated to 12 hex characters) — never a raw ID. This is implemented as:

- `conversationFingerprint`/`diffConversationIDs`/`compareMembership` (`main.go`) — pure, unit-tested functions. `compareMembership` is the decisive-test primitive (README Phase D): a clean single-item disappearance between a baseline and a post-pin enumeration is the Outcome B signal; no change is the Outcome A signal; more than one item changing is reported as not attributable to a single controlled action, matching Outcome C ("NOT VERIFIED").
- A new bridge method, `pins`, passively captures the observed, undocumented `/backend-api/pins` endpoint the same way every other endpoint in this bridge is captured (`bridge/main.js`'s `captureTarget`/`attachCapture`), sanitized by a new `pinsFrom` (`bridge/preload.js`) that recognizes only conversation-ID-shaped values via a deep object walk (the pins payload's exact shape was unconfirmed at design time) — never a pinned item's title. A `pinsCaptureID` counter lets the caller distinguish a genuinely fresh observation from a stale one, addressing the "avoid stale/cached data" requirement (Phase E) for the pins side of the evidence.
- `runPinExperiment` (`main.go`, behind a new `-pin-experiment` flag) drives the full interactive cycle end to end: baseline enumeration + pins snapshot → prompt the operator to pin one conversation and refresh the real UI → re-enumerate and compare → look for any passively-observed `is_starred=true` capture and check whether the affected conversation appears there (the secondary question, Phase F) → prompt the operator to unpin/restore → re-enumerate once more and confirm the series returned to its exact baseline membership. It never issues a pin/star mutation itself; every mutation step is an explicit human action via the real ChatGPT UI, as the safety boundary requires.

All of the above was implemented and verified structurally (`go build`, `go vet`, `go test -race`, `node --check` on both bridge files — see Verification below) in this same environment, which lacks kitty graphics support and therefore cannot itself drive `terminal-browser --app-mode` interactively. **The live experiment itself (`go run . -project-name agentsctl -pin-experiment`, run interactively by the repository owner from a kitty-capable terminal, performing the pin/unpin steps in the real ChatGPT UI as prompted) has not yet been executed** — consistent with this spike's established methodology (Phases 1, 4-9 all required the same human hand-off) and with the safety boundary's requirement that pin/star mutations only ever happen via explicit human UI action. Until that live run happens, `activeListCoverageCaveat` and `Coverage: UNKNOWN` remain exactly as the classifier hardening pass left them — this pass adds the capability to resolve the caveat, it does not itself resolve it.

**Implication:** The `is_starred` blocker's resolution now depends only on one interactive live run, not on further design or implementation work. Whoever performs it should record, in addition to the program's own output: the exact UI label used to pin (Pin/Star/Favorite/other — the program cannot observe UI semantics beyond DOM structure it already checks for unrelated purposes, so this is an operator observation), and should update the Results table below and re-run the classifier/coverage tests if the live evidence changes what `activeListRequiredSeries`/`classifyForActiveList` should treat as safe.

**Sixth pass — first live run was inconclusive; added an explicit freshness gate (2026-09-11, same day):** the repository owner ran `go run . -project-name agentsctl -pin-experiment` against the real account and performed both prompted actions (pin, then unpin, each followed by a UI reload/navigation as instructed). The raw output showed `membership evidence (pin): disappeared=[] appeared=[]` and `restore evidence: PASS`, which at first glance reads as Outcome A. It is not: the tool's own `after-pin pins: ... fresh=false` line (the pins freshness flag added in the fifth pass) showed the `/backend-api/pins` capture never advanced past its pre-experiment `captureID`, and closer inspection of the printed `global conversations wire capture` lines showed the *same* `series`/`digest` pair (`ea86234cf48d` / `b9220daa9e59`) repeated identically across the baseline, after-pin, and after-restore phases — no new `/backend-api/conversations` response was ever captured for the rest of the run after the page's initial settle. `enumerateAllConversations`'s own `seenCaptures` bookkeeping is local to each call, so it silently re-derives the same result from the bridge's replayed capture history even when nothing new was fetched; a "no membership change" result under those conditions is not evidence of anything — it is indistinguishable from "no new data was ever fetched," which is exactly what happened.

This was a real gap in the fifth pass's tooling, not just an unlucky run: only the pins side had a freshness check, the target-series side did not. Fixed by adding `maxObservedCaptureID` (queries the bridge's full `/backend-api/conversations` capture history and returns the highest `CaptureID` observed, across any query shape — not just MATCH-classified target-series ones) and `membershipOutcome` (pure, unit-tested), which now refuses to report Outcome A or B unless the captureID watermark actually advanced between the two observations being compared; otherwise it reports `NOT VERIFIED` explicitly, with the reason. `runPinExperiment` now prints an explicit `conversations capture watermark: baseline=... after_pin=... fresh=...` line before every membership verdict, and the restore-phase PASS/NOT VERIFIED verdict is gated the same way.

**Why the refresh likely failed:** the prompt's "reload or navigate within the Project sidebar" instruction does not, on its own, guarantee a network refetch of this specific query — ChatGPT's sidebar is a client-rendered SPA that may serve cached in-memory state for a view it already has data for, and the debugger capture only sees what the browser actually sends over the network. A follow-up run should use a more forceful refresh: a hard reload (not just in-app navigation), or navigating away to an entirely different Project/route and back, to force a genuine re-fetch — and should confirm the tool's own `fresh=true` line before trusting any membership verdict it prints.

**Result: C. NOT VERIFIED** (unchanged from the fifth pass, now for a directly-observed reason instead of "not yet run"). The live run this pass performed did not produce trustworthy evidence either way, and the tool itself now says so explicitly rather than reporting a misleading Outcome A. A re-run with a more forceful refresh action is still needed.

**Seventh pass — second live run exposed a watermark-timing bug in the sixth pass's own fix (2026-09-11, same day):** a second live run (same account, same command) printed `conversations capture watermark: baseline=14 after_pin=15 fresh=true` and `outcome: A candidate`. That reading was itself a false positive. `runPinExperiment` measured `baselineMaxCapture` *before* calling `enumerateAllConversations` for the baseline phase — but `enumerateAllConversations` drives its own scroll-simulation attempts internally, which can advance the bridge's capture history on their own, independent of anything the operator does. In this run, exactly that happened: the baseline enumeration's own scrolling produced a new capture (watermark 14→15) *during* the baseline phase, before the operator had even been prompted to pin anything. Because the "after pin" watermark was measured after the operator's action while the "baseline" comparison value was frozen from *before* baseline's own work, the comparison spuriously read as advancement caused by the pin action. Direct evidence this was spurious: the "after pin" phase's own printed capture list topped out at capture #15 — the same capture baseline's own scrolling had already produced — with no capture #16 anywhere, i.e. zero new captures were observed as a result of the operator's actual pin+reload action.

Fixed by measuring each phase's watermark *after* that phase's own `enumerateAllConversations` call returns (once its internal scrolling has already settled), rather than before the next call starts, so every comparison reflects genuinely new captures observed strictly between two settled states, not an enumeration's own internal probing misattributed to the operator's action. The `baseline target series` line now also prints its own `capture_watermark` for direct inspection. This is a build-vs-runtime-verified fix (`go build`, `go vet`, `go test -race`, `node --check` all pass) — the corrected code has not yet been re-run live.

**Second live run, corrected interpretation:** with the bug now understood, the second run's *restore* phase comparison (`after_pin=15 after_restore=15 fresh=false`, correctly reported `NOT VERIFIED`) was measured consistently on both sides (both watermarks taken before their respective next enumeration started, and neither side's own scrolling happened to move the needle) and remains valid: no fresh capture was observed after the restore action. The *pin*-phase comparison is unusable due to the bug above and must be disregarded — it does not support Outcome A, contrary to what it printed at the time.

**Result: C. NOT VERIFIED**, still. Two live runs so far have each surfaced a genuine bug in this experiment's own evidence-gathering rather than producing trustworthy membership evidence either way; both bugs are now fixed. A third live run, with the corrected watermark timing, is needed before this decision can change.

**Eighth pass — three remaining evidence-isolation gaps found on review, before a third live run (2026-09-12):** a review of the sixth/seventh-pass fix, ahead of running it live again, found three more ways this experiment's evidence could still be untrustworthy even with the timing bug fixed:

1. **The freshness watermark was not target-specific.** `maxObservedCaptureID` (all-series) advances when ANY `/backend-api/conversations` query is freshly captured — including an irrelevant one, e.g. `hide_snorlax=true` or `is_archived=true`. A `fresh=true` reading from it never actually proved the *active-list target series itself* (`hide_snorlax=false/absent`, `is_archived=false`, `is_starred=false`, `order=updated`) was refetched. Fixed with `maxMatchCaptureID` and `selectFreshMatchDiagnostics`, both pure functions that reuse `classifyForActiveList` — the existing, already-proven source of truth for the target-series definition — rather than re-implementing the filter. `maxObservedCaptureID` is retained only as an auxiliary diagnostic printed alongside, never as the freshness signal a decision is based on.

2. **Phase history was not isolated.** The general-purpose `enumerateAllConversations` folds together every capture the bridge has EVER observed for a series into one `mergeProjectPages` call. That is exactly correct for ordinary multi-page completeness enumeration (a real page can legitimately take many further scroll-triggered requests to reach exhaustion), but wrong for this experiment: a pre-mutation and a post-mutation capture of the *same* `SeriesKey`+`Offset` are two legitimate, different point-in-time snapshots, and folding them into one enumeration call trips `mergeProjectPages`'s "same page reported a different raw identity across observations" schema-drift check — treating the exact signal a controlled mutation should produce as if it were corruption. Fixed with a new, experiment-specific primitive, `targetSnapshotAfter`, which only ever considers captures strictly newer than an explicit `minCaptureIDExclusive` watermark (via `selectFreshMatchDiagnostics`), so two different phases' captures can never reach the same `mergeProjectPages` call together. It does not attempt general pagination completeness — this Project's target series is known to fit in one page, and the experiment only needs one trustworthy fresh snapshot per phase — and its own `Exhausted` field is documented as describing only that phase's own fresh pages, never a general completeness claim. `enumerateAllConversations` itself, and its existing consistency invariants, are unchanged; `runPinExperiment` no longer calls it at all.

3. **The controlled conversation was identified the wrong way for the important case.** The previous design inferred which conversation was affected from the target-series set difference — but a target-series set difference identifies a candidate only when something *disappears* (Outcome B). When nothing disappears (Outcome A — the actual hypothesis this experiment is trying to confirm or refute), "disappeared=[]" gives no candidate ID to check membership against; it is not really evidence of anything specific. Fixed by identifying the controlled conversation from the `/backend-api/pins` before/after delta instead (`controlledSampleFromPinsDelta`): it requires a confirmed-fresh pins capture and a clean single addition (exactly one added ID, zero removed) before it will name a controlled ID at all; any other shape (no advance, zero or multiple additions, any removal) returns `NOT VERIFIED` rather than guessing. The old set-difference-based `compareMembership` is retained only as an auxiliary, non-decisive diagnostic ("did anything else in the target set change") — never the basis for an A/B verdict.

The decisive test itself is now two pure functions, `pinExperimentOutcome` and `pinExperimentRestoreOutcome`, each requiring every trust condition together (a successfully-identified controlled sample, that sample confirmed present in the baseline snapshot, and a fresh target-series MATCH snapshot to check it against) before returning anything but `NOT VERIFIED` — unit-tested for every combination the task specified (target watermark ignoring irrelevant series, phase isolation via two independent `mergeProjectPages` calls that individually succeed while a deliberately-mixed call still fails closed, clean/ambiguous/non-fresh/removal-present pins deltas, and both outcome functions' A/B/PASS/NOT VERIFIED branches).

`runPinExperiment` also now prints, at every phase transition: the target-specific watermark (`before=... after=... fresh=...`) alongside the auxiliary all-series watermark for comparison, and `printCaptureDiagnostics` (attached debugger count, matched response count, capture error count) — specifically so a `fresh=false` result can be told apart between "the debugger/capture mechanism itself is not working" and "capture is healthy but the target series specifically was not re-requested," per the task's diagnostic requirement. No credential or private data crosses any of these — `pingInfo` was already a pure health/count summary.

None of this changes the decision: **Coverage remains UNKNOWN, `is_starred` coverage semantics remain NOT VERIFIED, and Phase 5 completeness remains CONDITIONAL** until a third live run, with all of these fixes in place, actually produces a confirmed controlled sample and a fresh target-series snapshot to test it against. If that live run still shows `target fresh=false` after a real hard reload, the task's own guidance is followed: this is recorded as `same-process UI refresh cannot currently force a fresh target-series observation`, and the next step is a fallback experiment (a fresh `terminal-browser` process against the same persistent partition, hypothesizing that ChatGPT's own boot sequence — not an in-page reload — would force a fresh target-series fetch), not another repetition of the same reload cycle.

**Ninth pass — third live run: clean, bug-free NOT VERIFIED; same-process reload does not force a fresh target-series or pins capture (2026-09-12):** the repository owner ran the corrected harness (`go run . -project-name agentsctl -pin-experiment`), performing the pin action followed by a hard reload, then the unpin action followed by another hard reload, exactly as prompted. Unlike the two prior runs, this one triggered no bug and no false reading:

- `target capture watermark: before=5 after=5 fresh=false` for the pin phase, and `before=5 after=5 fresh=false` again for the restore phase — the active-list target series (`hide_snorlax=false/absent`, `is_archived=false`, `is_starred=false`, `order=updated`) was never freshly captured after either action.
- `after pin target series: target_count=0 fresh_pages_used=0` — this is the corrected, honest behavior from the eighth-pass phase-isolation fix: with zero fresh MATCH captures, the phase-scoped snapshot is genuinely empty rather than silently falling back to baseline's stale data (which is exactly what made the earlier, buggy runs misleadingly look like "no change").
- `after-pin pins: captureID=5 ... fresh=false` and `after-restore pins: captureID=5 ... fresh=false` — no fresh `/backend-api/pins` capture was observed after either action either.
- Because neither the pins nor the target watermark ever advanced, `controlledSampleFromPinsDelta` correctly refused to name a controlled sample, and both `pinExperimentOutcome`/`pinExperimentRestoreOutcome` correctly returned `NOT VERIFIED` — the harness did exactly what it was built to do.
- The `all_series_capture_watermark` DID advance during the same window (17→25, then further during the restore phase's own scroll attempts) and `printCaptureDiagnostics` confirmed the capture pipeline was healthy and active throughout (`attached_debuggers=1`, `matched_responses` climbing 72→80→85, `capture_errors` flat at a pre-existing 8, not increasing) — ruling out cause (A) "the debugger/capture mechanism itself is not working." Every one of those newly-advancing captures was `classification=DEFINITELY_IRRELEVANT hide_snorlax=true`, continuing that OTHER series' own pagination (`offset` climbing 340→368→...→536, then further after restore) — i.e., this session's sidebar-scroll simulation kept advancing the Project-*excluding* series, exactly as every prior phase of this spike observed, and never touched the target series at all. This is cause (B): capture is healthy, but the active-list target series specifically was never re-requested.

**A plausible, evidence-based root cause, not yet tested as its own variable:** the scrolled series being `hide_snorlax=true` (the general/global chat list) rather than the Project-scoped target series suggests the visible, scrollable sidebar view after the operator's hard reload was ChatGPT's general home view, not the Project's own conversation list — a hard reload alone may return to `https://chatgpt.com/` rather than staying inside the Project. If so, the missing variable is not "reload vs. no reload" but "does the operator explicitly re-enter the Project view afterward," which was listed in the original task's Phase E candidates ("Project を一度離れて戻る", "Project navigation") but not isolated as its own test in this run.

**Result: C. NOT VERIFIED**, unchanged, but now for the first time from a run with no known bug in the harness itself. Per the task's explicit instruction, this same pin→hard-reload cycle is not being repeated again automatically. `same-process UI refresh cannot currently force a fresh target-series observation` is recorded as the finding from this run. Two paths forward were identified and left as an explicit decision for the repository owner rather than assumed: (1) one more, differently-targeted live action — explicitly navigating into the Project view (not just reloading) before checking freshness, which is a cheap variation of the existing harness, not a new one; or (2) the fallback experiment named in the task (start a fresh `terminal-browser` process against the same persistent partition, hypothesizing that ChatGPT's own boot sequence forces a fresh target-series fetch that an in-page reload does not) — not implemented this pass, per the task's explicit "does not need to be implemented/executed right away" allowance.

**Tenth pass — fresh-process fallback implemented (2026-09-12):** the repository owner chose to proceed with the fallback rather than trying one more same-process variant. Implemented as `browserProcess` (bundles the `terminal-browser` child process with its owning pseudo-PTY as a unit that can be started, stopped, and restarted — `startBrowserProcess`/`(*browserProcess).stop`) and `restartBrowserProcess`, which stops the current process and starts an entirely new one against the *same persistent partition* mid-run. `runPinExperiment` now calls this between phases instead of asking the operator to reload: pin (operator action, browser left running) → restart → fresh snapshot; unpin (operator action) → restart → fresh snapshot.

This is deliberately simpler than a cross-process design: only the *child* `terminal-browser` process is replaced, not the Go orchestrator (`go run .`) itself, which stays alive for the whole experiment. That means baseline's conversation list and pins snapshot — already held in ordinary Go variables — remain valid for comparison against the new process's fresh observations with no serialization, no disk, and no cross-process handoff at all; the task's concern about not persisting a controlled conversation's raw ID to disk/log is satisfied by construction, not by an explicit fingerprint-only protocol, since nothing ever leaves this one process's memory. `targetSnapshotAfter` needed no changes to support this: called with watermark 0 against a freshly-restarted process (which has no prior capture history of its own at all), "fresh" trivially and correctly means "any MATCH capture observed in this process." `maxObservedCaptureID`, which only made sense for comparing watermarks *within* one continuously-running process, became dead code under this design and was removed along with the same-process watermark-comparison prints; `printCaptureDiagnostics` (attached debugger count, matched responses, capture errors) is kept and printed once per fresh process instead, for the same "is the capture pipeline itself healthy" diagnostic purpose.

One more piece of live evidence shaped this pass: the ninth-pass run's scroll-simulation captures kept advancing the `hide_snorlax=true` (general/global chat list) series' own pagination and never touched the target series at all, suggesting the visible view after a plain reload was ChatGPT's general home view, not the Project's own list. So after every restart, `runPinExperiment` now also calls the existing `conversations` bridge method for the configured Project once (`discoverConversations`) — which `bridge/main.js` already navigates to the Project page for whenever nothing is cached yet, true by construction right after a restart — to actively force the app into the Project view before checking for a fresh target-series capture, rather than relying on a bare reload alone.

This is a build/test-verified implementation (`go build`, `go vet`, `go test -race`, `node --check` all pass) — it has not yet been run live. **Coverage remains UNKNOWN, `is_starred` coverage semantics remain NOT VERIFIED, and Phase 5 completeness remains CONDITIONAL** pending that live run.

**Eleventh pass — first live run of the fallback crashed the restarted process (2026-09-12):** the repository owner ran the fresh-process fallback. The restart itself reported `browser process restarted: PASS`, but the very next call (`discoverConversations`, the forced Project-navigation step) failed with `write: broken pipe`, and the following step failed the same way — the new `terminal-browser` process answered its initial post-restart `ping` successfully, then the connection died within a few seconds.

**Hypothesis:** `terminate()` sending SIGTERM and `cmd.Wait()` returning only confirms this program's direct child process has exited — it does not guarantee every resource the old process held (in particular, the persistent partition's own Electron/Chromium lock file) has actually been released the instant `restartBrowserProcess`'s call to `old.stop()` returns. Starting a new instance against the same partition too soon after the old one's exit is a plausible race for exactly this symptom (an initially-healthy process crashing shortly after startup). This has not been independently confirmed (e.g. from `terminal-browser`'s own logs) — it is the most likely explanation given the timing, not a proven root cause.

**Mitigation:** added a 3-second settle delay after `old.stop()` returns and before starting the new process, and — since the live failure was specifically "healthy at first ping, dead a few seconds later" — a second liveness re-check after the existing post-ping settle sleep, so a repeat of this exact failure is caught immediately with a clear diagnostic (`"browser process died shortly after restart, likely a partition lock conflict..."`) rather than surfacing later as a confusing `broken pipe` from an unrelated call several steps into the experiment. This is a best-effort, evidence-based mitigation for an environment-specific process-lifecycle issue this spike cannot directly inspect (no access to `terminal-browser`'s own internals or logs) — build/test-verified, not yet re-run live.

**Result: C. NOT VERIFIED**, still — this run never reached the decisive membership test at all, since the process crash happened before any post-restart target-series or pins capture could be attempted. Coverage/Phase 5 status unchanged.

### Phase 6: Chat versus Work discrimination

**Hypothesis:** Sanitized list/detail metadata contains an explicit discriminator that differs between known Chat and Work samples.

**Method:** With a human-created Work/Agent session now confirmed to exist in the Project (found via the Phase 5 correction), ran the same field/cardinality comparison across all 18 known conversations (5 from the project-scoped list + 13 newly discovered), then extended the sanitizer to also report the redacted distinct *values* for marker fields (still never conversation content, titles, or per-conversation identity mapping — see Known limitations for a mid-run correction to this).

**Observed:** Two fields changed shape only once the 13 newly-discovered conversations were included:
- `messages.[].metadata.async_source` — absent from all 27 fields observed in the original 5-sample evidence; present in exactly 1 of the 18 combined conversations, holding an internal backend worker/server identifier (`saserver-<region>-prod...:conversation-turn-...`). The field name itself denotes asynchronous/agentic execution infrastructure.
- `default_model_slug` / `messages.[].metadata.model_slug` — `distinct_values` rose from 1 (`gpt-5-6-thinking` in all 5) to 2, with the second value present outside the original 5. The naming (`sol-wm`) is consistent with a distinct backend/model variant for agentic work.

A weaker, ambiguous third signal: `conversation_origin` also gained a second value (`tpp`) outside the original 5, but that label doesn't obviously mean "Work" (more likely "third-party plugin" or similar) and shouldn't be relied on without independent confirmation. The originally-hypothesized `async_status` field stayed `null` across all 18 samples, including whichever one carries `async_source` — it is very likely a transient "currently executing" flag that resets once a Work session finishes, not a durable discriminator.

An Open-URL probe (Phase 7) further confirmed that navigating to the specific conversation ID carrying `async_source` behaves identically (readiness, no login redirect) to navigating to a known plain Chat ID, which is consistent with — though does not by itself prove — that ID being the created Work session.

**Result: CONDITIONAL — B. A discriminator candidate exists but is undocumented and fragile.** `messages.[].metadata.async_source` presence is the primary candidate; `model_slug`/`default_model_slug` value is a corroborating secondary signal. This is evidence from a single Work sample (n=1) against an internal, unstable field name — not the "A. stable explicit discriminator" bar this phase set out to clear.

**Implication:** This is the primary remaining acceptance blocker, but it moved from "cannot distinguish" to "a plausible undocumented discriminator, needs corroboration." Before this is production-ready: confirm `async_source` presence (or absence) across more than one Work sample, confirm it does not also appear on ordinary tool-using Chats (e.g. browsing/code-interpreter turns, which already use several of the same marker-named fields), and treat any adapter built on it as versioned/fragile per the security boundary (ChatGPT can rename or remove it without notice).

### Phase 7: canonical Open behavior

**Hypothesis:** Conversation identity maps to one stable Project-preserving URL for both Chat and Work.

**Method:** Extended the harness with an `openURLProbe` bridge method that, without ever transmitting the chosen conversation ID across the socket, selects one conversation already showing the `async_source` marker (Phase 6) and one plain conversation from the original 5, then navigates to both `/c/{id}` (canonical) and `/g/{project_id}/c/{id}` (project-scoped) for each and reports readiness, login-redirect state, and an ID-redacted comparison of the resulting URL against the requested one.

**Observed:** All four navigations reached `ready=complete` with `login_prompt=false` — no login-redirect in any case, for either conversation kind or either starting URL shape. The exact-string comparison against the requested path initially reported `false` in all four cases; the ID-redacted diagnostic showed why: every navigation — including the plain `/c/{id}` canonical form — resolved to the same `/g/g-p-<redacted>/c/<conversation-id>` shape. Cross-checked against the Project URL the human originally shared for this spike (`https://chatgpt.com/g/g-p-<redacted>-<project-name-slug>/c/<conversation-id>`), the harness's own candidate Project URL (`https://chatgpt.com/g/{id}`, built from the bare `g-p-...` ID only) was itself incomplete — ChatGPT's real canonical form appends a human-readable Project-name slug after the ID. The exact-match probe was comparing against that incomplete candidate, not evidence that Open failed.

**Result: PASS**, with a resolved strategy: **bare `/c/{conversation_id}` is the canonical Open URL for both Chat and Work.** ChatGPT itself resolves/redirects a bare conversation ID (and even an ID-only, slug-less project-scoped guess) to the fully-qualified, Project-slug-including URL, uniformly for the plain Chat sample and the `async_source`-marked sample, with no login redirect in either case.

**Implication:** Production code does not need to resolve or track the Project's URL slug at all — `https://chatgpt.com/c/{conversation_id}` is sufficient as the Open target and lets ChatGPT's own routing normalize the rest. This removes a category of URL-construction fragility the original candidate list assumed.

### Phase 8: app-mode session view

**Hypothesis:** Official ChatGPT Web UI is usable as the attach-equivalent view.

**Method (sandboxed harness):** Attempted a foreground `terminal-browser` launch in the current terminal; app-mode requires the same graphics path.

**Observed (sandboxed harness):** The terminal was rejected as unable to show images. Not exercised further here.

**Method (human terminal, iTerm2 with Kitty graphics protocol support):** `terminal-browser open --app-mode --partition=agentsctl-chatgpt https://chatgpt.com`, against the same authenticated partition used throughout. The human viewed both a normal Chat and the Work session created for Phase 6/7, then sent a real message through the in-browser composer.

**Observed:** Both Chat and Work sessions rendered and were browsable normally. A message was sent successfully through the composer. One concrete UX defect surfaced: `terminal-browser`'s key handling does not distinguish an IME composition-confirm `Enter` from a send `Enter` — composing a prompt with an IME (e.g. Japanese) and pressing `Enter` to confirm the conversion submits the message prematurely instead of confirming the conversion. The workaround is to compose the message elsewhere and paste it in.

**Result: PASS, with a known UX defect.** Transcript rendering, browsing between Chat and Work, and message sending all work. IME-based composition does not work correctly in the terminal-embedded browser.

**Implication:** Official ChatGPT Web UI is usable as the attach-equivalent view for both Chat and Work. The IME defect is a real usability blocker for any user who composes in an IME-dependent language directly inside the terminal-embedded window — it should be reported upstream to `zenbu-labs/terminal-browser` and tracked as a UX caveat in any production design, not silently accepted.

### Phase 9: return with Ctrl+]

**Hypothesis:** A preload handler can close only the browser view while cloud execution continues.

**Method (sandboxed harness):** Added an opt-in capture handler using the documented `globalThis.terminalBrowser.quit()` API when `AGENTSCTL_CHATGPT_CLOSE_KEY=1`. No visible authenticated Work execution was available for an end-to-end test in that environment.

**Method (human terminal, iTerm2):** `AGENTSCTL_CHATGPT_CLOSE_KEY=1 terminal-browser open --app-mode --partition=agentsctl-chatgpt --preload=$(pwd)/bridge/preload.js https://chatgpt.com`, navigated to the Work session, pressed `Ctrl+]`, then reopened and reselected the same session.

**Observed:** `Ctrl+]` closed only the `terminal-browser` view; control returned to the shell with no other apparent side effects. On reselecting the same Work session afterward, its running/completed state was preserved exactly as before the view was closed — nothing was reset or re-run. (An initial reopen against the bare `https://chatgpt.com` root URL showed a new-chat screen, as expected for that URL — that was a methodology artifact, not evidence of lost state; reselecting the specific session from the sidebar showed the true, preserved state.)

**Result: PASS**

**Implication:** The `Ctrl+]`-closes-view-only, cloud-Work-continues semantics that Issue #7 wants for ChatGPT (as opposed to Claude/Codex's PTY-detach semantics) are achievable with a small opt-in preload handler over the documented `terminalBrowser.quit()` API, and cloud continuity was directly observed, not just inferred.

### Phase 10: failure behavior

**Hypothesis:** Missing dependencies, stopped lifecycle owners, logged-out state, bridge errors, and malformed responses fail closed.

**Method:** Used a nonexistent binary, closed the PTY during a hold, queried while logged out, and ran response-validation tests for remote rejection, missing results, and mismatched response IDs.

**Observed:** Each case returned an error. The harness did not emit partial Project/session data and did not attempt credential extraction or another authentication route.

**Result: PASS for exercised cases; other endpoint/schema cases remain NOT VERIFIED live**

**Implication:** The boundary can fail closed, but authenticated invalid-ID and live schema-change probes remain to be run.

## Results

| Phase | Result |
| --- | --- |
| 0. Baseline | CONDITIONAL |
| 1. Persistent authentication | PASS (proven by authenticated fetch after restart) |
| 2. Background helper | CONDITIONAL — PoC workaround only |
| 3. Browser bridge | PASS |
| 4. Project discovery | PASS (name→ID resolution); rename/duplicate-name robustness NOT VERIFIED |
| 5. Session discovery | CONDITIONAL, method corrected twice — global `/backend-api/conversations` filtered by Project required (project-scoped endpoint under-reports, 5 of ~16-17); pagination follow-up (2026-09-11) identified the `hide_snorlax` filter and offset/limit contract, but could not prove complete enumeration beyond one page — see the Phase 5 addendum |
| 6. Chat / Work discrimination | CONDITIONAL — B. `async_source` field presence is a plausible undocumented discriminator (n=1 Work sample) |
| 7. Stable identity and Open | PASS — bare `/c/{conversation_id}` is sufficient; ChatGPT normalizes to the full Project-slug URL itself |
| 8. App-mode UX | PASS, with a known UX defect — Chat/Work both render and are usable; IME composition (e.g. Japanese) submits prematurely on the conversion-confirm `Enter` |
| 9. Ctrl+] semantics | PASS — view closes cleanly; reselecting the Work session shows state preserved exactly, not reset |
| 10. Failure behavior | PASS for exercised cases |

## Capability matrix

`Proven` means observed in this run. `Prepared` means the PoC has a guarded path that could not be exercised due to missing authentication.

| Capability | Programmatic | Browser UI | Stability | Notes |
| --- | --- | --- | --- | --- |
| List | Conditional (Project resolution proven; enumeration completeness not proven) | N/A | Undocumented / unstable, pagination contract only partially exercisable | Project-scoped endpoint under-reports; the global endpoint's `hide_snorlax` filter must be false/absent for Project conversations to appear at all; the mechanism that includes them was never observed advancing past its first page (self-fetch pagination is unauthorized — HTTP 401 — and the real UI's own scroll-driven advancement was only observed on the Project-*excluding* query series); `/backend-api/tasks` investigated and rejected as unrelated |
| Open | Proven | N/A (delegates to browser UI) | Undocumented navigation behavior, but consistent across 4 probes | Bare `/c/{id}` normalizes correctly for both Chat and a Work-marked sample; no login redirect |
| Read transcript | Out of scope | Proven | Stable enough for delegation | Rendered correctly for both Chat and Work in app-mode |
| Send message | Out of scope | Proven, with a caveat | IME input does not work correctly | A real message was sent successfully; IME composition (e.g. Japanese) submits prematurely on the conversion-confirm `Enter` — compose-and-paste is the workaround |
| Continue Chat | Out of scope | Proven | Stable enough for delegation | Reopened and continued normally |
| Continue Work | Out of scope | Proven | Stable enough for delegation | Reselecting the Work session after a `Ctrl+]` close showed state preserved exactly |
| Observe Work state | Conditional — B (undocumented) | Proven (visually, via UI) | Fragile programmatically; fine via UI delegation | `messages.[].metadata.async_source` presence is a plausible programmatic marker (n=1 sample); top-level `async_status` stayed null and is likely transient. Visually, the UI itself shows Work progress/state correctly, so UI delegation does not depend on solving the programmatic discriminator |
| Rename | Out of scope | Not verified | Unknown | No destructive or mutating probe |
| Archive/delete | Out of scope | Not verified | Unknown | No destructive probe |

## Known limitations

- Stock `terminal-browser` has no proven display-free service lifecycle. The PTY is a required liveness owner.
- The sandboxed harness's terminal cannot display terminal-browser graphics; Phases 8 and 9 were exercised instead from the repository owner's iTerm2 (Kitty graphics protocol support) and passed. `terminal-browser`'s key handling does not distinguish an IME composition-confirm `Enter` from a send `Enter`, so IME-based composition (e.g. Japanese) submits prematurely; this should be reported upstream and treated as a known UX caveat, not solved by this spike.
- Authentication, Project discovery, and session discovery (Phases 1, 4, 5) are proven against the human's real account and real Project via a human-operated terminal, after one manual login. Session discovery required correcting the method mid-run: the project-scoped conversations endpoint returns an incomplete list (5 of ~16-17), so a production adapter needs the global, Project-filtered endpoint too.
- **Session discovery completeness (Phase 5 pagination follow-up, 2026-09-11):** the global endpoint's own pagination was never verified in the original Phase 5 round-2 result — that result was one unverified response, not a proven-complete one. Live investigation found: (1) a previously-undocumented `hide_snorlax` query flag that must be false/absent for Project conversations to appear in `items` at all — the original method never checked this, so its correctness was accidental/timing-dependent, not verified by construction; (2) the endpoint's `total` field is not trustworthy as an exhaustion signal (observed identical across differently-filtered pages, and observed jumping 29→57 between two requests moments apart in the same session); (3) the bridge cannot self-issue further authenticated pages — a same-origin `fetch` from the bridge's own script returns HTTP 401 in every attempt, so pagination can only be driven by asking the real ChatGPT UI to scroll and passively harvesting the result; (4) the real UI's own scroll-driven pagination was only ever observed advancing the Project-*excluding* (`hide_snorlax=true`) query series — the Project-inclusive series was never observed advancing past `offset=0` in any run. A new `mergeProjectPages`/`enumerateAllConversations` implementation (unit-tested, `main_test.go`) fails closed on this: it reports `INCOMPLETE`, not a fabricated complete list, whenever exhaustion (a page shorter than its own limit) is not observed. Complete enumeration is proven only for a Project small enough to fit in one page (this account: 16, within the observed limit of 28); it is not proven to scale beyond that, and no mechanism in this PoC can force or verify the beyond-one-page case.
- **Association schema-drift guarantee (2026-09-11, second pagination hardening pass):** the enumeration fails closed if a raw conversation item exposes neither `gizmo_id` nor `project_id` as an own property, based on live evidence (several captures, 28 raw items each, this account) that every item — Project-associated or not — always carries one of these keys, present but `null` for an ordinary non-Project chat. This is an empirical observation from one account's current schema, not a documented ChatGPT API guarantee; it is deliberately kept alongside, not instead of, the narrower and more concretely-grounded known-ID cross-check (a conversation ID already confirmed to belong to the configured Project whose association no longer resolves to it). Both are undocumented-schema assumptions that ChatGPT could change without notice, consistent with everything else in this section.
- Chat/Work discrimination (Phase 6) has moved from "no evidence" to "one undocumented candidate field (`async_source`) confirmed on a single human-created Work sample." It is not yet confirmed absent from ordinary tool-using Chats, and is not a documented, stable API guarantee.
- **Mid-run correction:** the field-value reporting added for Phase 6 initially printed unredacted `g-p-...` Project IDs and full conversation/turn UUIDs when they appeared as a structural field's *value* (e.g. `conversation_template_id`, `working_turn_id`) rather than as a key. The existing ID-redaction (already applied to `observedBackendPaths` and the Open-URL diagnostic) was not applied to this path. This was caught during the same session, before any further extraction, and fixed by routing all reported values through the shared redaction helper while still computing distinct-value counts from the raw (unredacted) values, so per-item uniqueness signals aren't lost. No cookie, token, or authorization header was ever involved; the exposed values were structural identifiers already visible to the operator from ChatGPT's own URLs. Any adapter built on this pattern must route every value that might contain an ID through the same redaction before logging.
- **Project URL cleanup (2026-09-11):** the `projects` sanitizer previously returned a `url` field built as `https://chatgpt.com/g/{project_id}` (a bare-ID guess). Phase 7 already showed ChatGPT's real Project URLs carry a human-readable name slug this harness never resolves, so that field was never canonical. It has been removed rather than renamed, since nothing downstream (Go's `project` struct never had a matching field) ever consumed it. The conversation Open strategy (`https://chatgpt.com/c/{conversation_id}`, Phase 7) is unchanged.
- All investigated `/backend-api/...` endpoints and schemas are observed, undocumented, and unstable.
- The preload sanitizer deliberately rejects unknown shapes; ChatGPT changes will disable discovery until the adapter is reviewed.
- v0.8.0 was tested while v0.8.1 was current.

## Security boundary

The browser partition is the sole owner of ChatGPT authentication. The Go process and socket protocol must never receive or log cookies, authorization headers, access tokens, refresh tokens, browser storage credentials, or complete request headers. Raw endpoint responses remain inside the preload; only explicitly selected Project/conversation metadata can cross IPC.

The socket is local and changed to mode `0600`. Requests are size-limited and method allowlisted. Fetches are same-origin, credential-preserving browser calls to fixed path shapes. Failure never falls back to credential extraction.

Structural field values (status/type/kind/mode/origin-style labels used for Chat/Work discrimination evidence) are permitted to cross the bridge, but only after ID-redaction — see the Phase 6 mid-run correction in Known limitations. Conversation content, titles, and per-conversation identity mapping are never logged, by design of the aggregate-only evidence comparison.

## Recommendation

**Current decision: CONDITIONAL GO.**

The bridge mechanism, persistent authentication, Project discovery, session discovery, canonical Open URL, app-mode UI delegation, and `Ctrl+]` view-close-without-stopping-Work semantics are now all proven against a real account and a real, human-created Work sample, from both the sandboxed harness and a kitty-capable human terminal. Two acceptance-blocking gaps remain. The first, unchanged: the Chat/Work discriminator (`async_source` field presence is a plausible signal but confirmed on only one Work sample against an undocumented, unstable field name). The second, newly sharpened by the 2026-09-11 pagination follow-up: **complete Project session enumeration is proven only for a Project small enough to fit in one page of the global endpoint (this account: 16 conversations, page size 28); it is not proven, and this PoC found no way to prove, that a larger Project's conversations beyond the first page are reachable at all** — the only pagination-advancing mechanism observed (real-UI scroll) was only ever seen advancing the query variant that structurally excludes Project conversations, and the bridge's own attempt to self-drive further pages was rejected with HTTP 401. The background helper lifecycle also remains a PTY workaround, not a production service lifecycle — though it is no longer the binding blocker for UI delegation, since app-mode itself does not need the pseudo-PTY (only discovery does).

```text
#6 implementation before blocker resolution: no
```

Before starting Issue #6:

1. Corroborate the `async_source`-presence discriminator against more than one Work sample, and confirm it does not also appear on ordinary tool-using Chats (browsing, code interpreter, etc., which already share several marker field names). Do not ship a heuristic confirmed on n=1.
2. Obtain a supported `terminal-browser` no-render/service lifecycle, or an explicit upstream commitment, suitable for production; the current pseudo-PTY is a PoC workaround only (Phase 2) and is needed for background discovery even though app-mode UI delegation itself works without it.
3. **Resolve session-discovery completeness beyond one page before relying on it in production.** Build the production adapter to require `hide_snorlax` false/absent (Phase 5 follow-up) — not just "the global endpoint filtered by Project," which is necessary but not sufficient — and to treat enumeration as `INCOMPLETE`/fail-closed (per `mergeProjectPages`/`enumerateAllConversations` in this spike) whenever the highest-observed page is not shorter than its own limit, never trusting `total`. Separately confirm with a real account that has more than one page (≈28+) of Project conversations whether the real ChatGPT UI has *any* user-driven action that advances the Project-inclusive query past `offset=0` — this spike's account did not have enough conversations to force that case, and if no such action exists, production completeness for large Projects is an open architectural question, not a solved one. Additionally (third hardening pass): scope the target series to the existing `session.Provider.List(archived bool)` semantics (`is_archived=false` for the normal list; treat an `archived=true` view, if ever added, as its own independently-proven series set — never merge the two universes), and directly test whether `is_starred=true` conversations are excluded from the default view before treating coverage as anything but `UNKNOWN` — this spike found only indirect evidence (a separate `/backend-api/pins` endpoint) either way.
4. Report the IME composition-confirm-`Enter`-sends-prematurely defect to `zenbu-labs/terminal-browser` upstream, and track it as a known UX caveat for any user who composes in an IME-dependent language.
5. Re-run against the selected/pinned `terminal-browser` version (v0.8.0 tested vs. v0.8.1 current at spike time) and document its compatibility window.

None of these require abandoning the browser-backed direction — they are scoped hardening and verification steps, consistent with CONDITIONAL GO. Do not add a production ChatGPT provider or change shared provider/session contracts from this spike alone.

