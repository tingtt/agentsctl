# ChatGPT browser-backed integration spike

## Purpose

This spike collects evidence for [Issue #7](https://github.com/tingtt/agentsctl/issues/7). It validates a browser-backed ChatGPT integration in which `terminal-browser` owns authentication and the official ChatGPT UI, while `agentsctl` consumes only the minimum Project / conversation metadata needed for its session catalog.

This is not the production provider planned by Issue #6. It does not change the shared provider/session contracts, Agent View, composer, dispatch, or existing providers.

All observed ChatGPT `/backend-api/...` interfaces are **undocumented and unstable**. Production code must fail closed on schema drift and must never extract browser credentials into Go.

## Final decision

**GO for Issue #6 implementation.**

The spike has proven, against a real authenticated account and Project:

- persistent browser-owned authentication,
- Project discovery and stable Project-ID configuration,
- complete Project session enumeration,
- stable conversation identity and canonical Open behavior,
- official-UI interaction for both normal Chat and Work,
- `Ctrl+]` closing only the browser view while cloud Work state continues,
- fail-closed behavior for the exercised failure cases.

The Project List mechanism is proven complete for the exercised account/session:

```text
GET /backend-api/gizmos/{project_id}/conversations?cursor=...

pages fetched: 4
page sizes: 10, 10, 10, 5
terminal cursor observed: true
unique conversations: 35
duplicates_observed: 0
known normal Chat present: true
known Work present: true
Project session enumeration: COMPLETE
```

~~The remaining acceptance blocker is Chat / Work discrimination (`messages[].metadata.async_source`, n=1 Work sample).~~

**Chat / Work discrimination is not required by the selected integration behavior.** Both normal Chat and Work are listed/opened as ChatGPT sessions through the same agentsctl catalog path, while interaction and state rendering are delegated to the official ChatGPT UI. The spike therefore has no remaining acceptance blocker before Issue #6 implementation.

Background/service lifecycle, the IME defect, terminal-browser version pinning, and cross-account re-verification remain production hardening / implementation concerns.

## Recommended production direction

```text
agentsctl
  │
  ├─ local session catalog / local pin state / created-at ordering
  │
  └─ mode-0600 Unix socket
        │
        ▼
terminal-browser persistent partition
        │
        ├─ main script: Electron / CDP bridge
        ├─ isolated preload: sanitize captured ChatGPT metadata
        └─ app-mode: official ChatGPT UI for interaction
```

Responsibilities:

- `terminal-browser` owns ChatGPT authentication and page execution.
- Raw ChatGPT responses stay browser-side.
- Go receives only allowlisted metadata.
- `agentsctl` owns local ordering and local pin state.
- ChatGPT remote pin/star state is ignored.
- Chat / Work interaction is delegated to the official ChatGPT Web UI rather than reimplemented in a native TUI.
- Chat / Work are not required to be represented as distinct provider/session types inside agentsctl.

## Configuration

Project name is suitable for one-time setup / display validation, but the stable configuration key should be the Project ID:

```toml
[chatgpt]
project_id = "g-p-..."
```

An optional `project_name` may be retained for display/validation, but must not be the resolver of record.

The directory containing the configuration remains the logical CWD for sessions from the configured ChatGPT Project so they can participate in existing `cwd` / `cwd/**` / `all` scopes.

## Session model decisions

### Identity

Stable provider-qualified identity:

```text
chatgpt:<conversation_id>
```

### Open

Canonical Open target:

```text
https://chatgpt.com/c/{conversation_id}
```

ChatGPT itself normalizes this to the Project-slug URL when appropriate. Production code does not need to discover or construct the Project URL slug.

### Chat / Work type

~~Use `messages[].metadata.async_source` to distinguish Work from normal Chat before implementation can proceed.~~

**No programmatic distinction is required.** The List/Open/attach-equivalent behavior is the same for both session kinds:

```text
Project conversation
  → agentsctl ChatGPT session
  → open official ChatGPT UI
```

Any ChatGPT-specific state or Work progress remains owned and rendered by ChatGPT itself.

The existing `async_source` investigation remains historical evidence only; it is not part of the production contract.

### Ordering

The cursor endpoint is observed to return sessions in updated-time order:

```text
created_desc=false
updated_desc=true
```

`agentsctl` must ignore server ordering and sort the final complete set locally by:

```text
created_at DESC
```

with a deterministic stable-identity tie-breaker.

### Pin state

ChatGPT remote pin/star state is intentionally not synchronized.

```text
ChatGPT pin/unpin
  → ignored by agentsctl local pin state

agentsctl Ctrl+T
  → local pin state only
  → no ChatGPT mutation
```

Local pin state is keyed by `chatgpt:<conversation_id>`.

## Complete List mechanism

### Endpoint

The recommended List source is the Project-scoped cursor endpoint:

```text
GET /backend-api/gizmos/{project_id}/conversations?cursor={cursor}
```

Initial entry point:

```text
cursor=0
```

Observed response fields used by the spike:

```text
items: [...]
cursor: <opaque string | terminal value>

item.id
item.create_time
item.update_time
```

No `total`/`count` field is used for completeness.

### Cursor discipline

Cursor values are opaque.

The implementation may:

- compare cursors for exact equality,
- detect cycles,
- pass a response cursor back to the next request verbatim.

It must not:

- parse cursor structure,
- increment cursors,
- compare cursor magnitude,
- infer a missing cursor.

Diagnostics expose only `cursor=0` or a short SHA-256 fingerprint, never the raw opaque value.

### Why passive capture is required

A browser-script self-fetch of the cursor-parameterized endpoint consistently returned HTTP 401. This is retained only as a negative probe.

The real ChatGPT frontend's own requests returned HTTP 200 and successfully paginated. Therefore the working path is:

```text
navigate Project view
  ↓
real ChatGPT frontend issues cursor request
  ↓
CDP Network capture observes response
  ↓
browser-side sanitizer
  ↓
Go receives allowlisted metadata
```

No cookie, access token, authorization header, anti-CSRF value, or full request header crosses the bridge.

### Series identity

`cursor` alone is not enough to identify a logical request series. Live traffic showed multiple structurally different requests sharing the same explicit `cursor=0`, including 5-item and 10-item first pages.

The spike therefore computes a `SeriesKey` from all query parameters **except `cursor`** and pins the selected series for the whole walk.

When multiple series are observed at entry, the spike selects the unique series whose first-page item count matches the independently observed Project-list link count in the DOM; ambiguity fails closed.

This eliminated the earlier false "same cursor, different conversation set" failures.

### Pagination trigger

Synthetic DOM `scrollTop` mutation was not reliable enough to reproduce the real frontend's pagination behavior.

The working input path mirrors terminal-browser itself:

- CDP `Emulation.setFocusEmulationEnabled({enabled:true})`,
- Electron `webContents.sendInputEvent` mouse-wheel input,
- platform wheel detent (`40px` on macOS, `120px` elsewhere),
- `hasPreciseScrollingDeltas: false`,
- `modifiers: []`.

A CDP `Input.dispatchMouseEvent` wheel path exists only as a fallback diagnostic; the successful full walk did not need it.

### Content-aware bottom tracking

A fixed number of wheel attempts was insufficient because `scrollHeight` grew whenever another page loaded.

Observed growth during the successful run:

```text
1133 → 1783 → 2433 → ...
```

The final driver therefore recomputes:

```text
maxScrollTop = max(scrollHeight - clientHeight, 0)
distanceToBottom = max(scrollHeight - clientHeight - scrollTop, 0)
```

on every round, continues toward the **current** bottom, and remains bounded by total wheel ticks / no-progress rounds.

This produced the full chain:

```text
transition 0→1: PASS
transition 1→2: PASS
transition 2→3: PASS
page 3: terminal cursor
```

### Completeness criteria

`COMPLETE` is reported only when all of the following hold:

1. enumeration starts from the selected series at `cursor=0`,
2. every continuation follows the previous response's own cursor,
3. no cursor cycle is observed,
4. every page passes schema validation,
5. a terminal page explicitly reports no next cursor,
6. every accepted page is accumulated,
7. conversations are deduplicated by stable conversation ID.

Bottom position, visible-link count, Project-filtered count, and any server `total` value are **not** completeness signals.

## Final live enumeration evidence

The thirteenth cursor-pagination live run on 2026-09-14 reached the spike's success condition:

```text
transition 0->1: expected_cursor_observed=true
transition 1->2: expected_cursor_observed=true
transition 2->3: expected_cursor_observed=true

pages fetched: 4
page 0: 10
page 1: 10
page 2: 10
page 3: 5
terminal cursor observed: true
unique conversations: 35
duplicates_observed: 0
failure category: COMPLETE
Project session enumeration: COMPLETE

response ordering:
  created_desc=false
  updated_desc=true

agentsctl ordering (creation time DESC) verified: true
known normal Chat present: true
known Work present: true
```

Every previously established invariant held throughout the full walk:

- selected `SeriesKey` remained pinned,
- opaque cursor discipline held,
- zero conversation duplicates were observed,
- cycle detection remained enabled,
- schema validation remained fail-closed,
- no browser credential material crossed into Go,
- remote ChatGPT pin state remained unused.

## Historical List investigation — superseded

Before the Project cursor endpoint was understood, the spike investigated the global endpoint:

```text
/backend-api/conversations
```

with `offset` / `limit`, `hide_snorlax`, `is_archived`, `is_starred`, and `order` query semantics.

Important historical evidence:

- `hide_snorlax=true` excludes Project/gizmo conversations.
- Project-filtered item count is not a pagination exhaustion signal.
- `total` was not trustworthy as an exhaustion signal.
- the main sidebar drove global offset pagination, but only for the Project-excluding series.
- self-issued global pagination fetches returned HTTP 401.
- a pin/unpin experiment proved only that first-page membership changed; it did **not** prove `is_starred=false` globally excludes pinned conversations.
- `is_starred=true` was never observed from real UI traffic.

Those findings led to:

```text
Coverage: UNKNOWN
Pagination: INCOMPLETE
Overall session discovery: INCOMPLETE
```

for the global path.

That path is now **superseded for production List purposes** by the Project-scoped cursor mechanism, which reached an explicit terminal cursor and returned 35 unique sessions. Do not resume the global `hide_snorlax` / `is_starred` / `offset=28` investigation unless the cursor mechanism itself regresses on a future account/session.

The earlier statement that the Project-scoped endpoint "returns only 5 sessions" is also superseded. The 5-item response was a different, smaller request series sharing the same URL path; it was not the complete cursor-paginated Project list.

## Historical Chat / Work discriminator investigation — not required

The spike investigated:

```text
messages[].metadata.async_source
```

as a possible durable Work marker. It was present in one human-created Work sample (`n=1`) and absent from the original plain-Chat sample set. `default_model_slug` / `messages[].metadata.model_slug` also changed alongside that sample; top-level `async_status` remained `null`.

~~Before production, corroborate `async_source` across multiple Work sessions and prove it absent from ordinary tool-using Chats.~~

That follow-up is no longer required because the selected product behavior does not branch on Chat-vs-Work type. Both are ordinary ChatGPT sessions from agentsctl's point of view, and the official UI owns their interaction/state differences.

## Browser interaction

The official ChatGPT Web UI is the attach-equivalent view.

| Capability | Result |
| --- | --- |
| List | **PASS** — complete Project cursor enumeration |
| Open | **PASS** — bare `/c/{conversation_id}` |
| Read transcript | **PASS via official UI** |
| Send message | **PASS via official UI**, IME caveat |
| Continue Chat | **PASS via official UI** |
| Continue Work | **PASS via official UI** |
| Observe state | **PASS via official UI**; no normalized Chat/Work type required |
| Rename | Out of initial provider scope / not verified |
| Archive/delete | Out of initial provider scope / not verified |

### `Ctrl+]`

`Ctrl+]` was verified to close only the `terminal-browser` view. Reopening the same Work session showed its cloud execution/session state preserved rather than reset or rerun.

ChatGPT therefore does not need the same provider-level detach semantics as local Claude/Codex PTYs; closing the view does not stop cloud Work.

### IME caveat

`terminal-browser` currently does not distinguish an IME composition-confirm `Enter` from a message-send `Enter`. Japanese IME composition can therefore submit prematurely.

Compose-and-paste works as a workaround. This should be tracked/reported upstream rather than silently baked into agentsctl behavior.

## Authentication

A dedicated persistent partition:

```text
agentsctl-chatgpt
```

preserved authenticated ChatGPT state across helper restarts after one manual human login.

Authenticated Project/sidebar fetches succeeded after restart, which is stronger evidence than DOM-only login signals.

Authentication remains browser-owned. The integration must never extract or persist:

- cookies,
- authorization headers,
- access/refresh tokens,
- browser credential/storage databases,
- complete credential-bearing request headers.

Long-horizon refresh behavior remains an empirical browser-session dependency because ChatGPT exposes no supported public contract for this integration.

## Project discovery

Project discovery through the observed sidebar response is **PASS** for the exercised account:

- authenticated Project enumeration worked,
- name→ID resolution worked unambiguously,
- stable configuration should use `g-p-...` ID.

Rename-survival and duplicate-name disambiguation were not explicitly exercised.

## Bridge and security boundary

Transport:

```text
Go
  ↓ mode-0600 Unix socket
terminal-browser main script
  ↓ Electron IPC
isolated preload in chatgpt.com
```

Rules:

- raw ChatGPT payloads stay browser-side,
- only allowlisted/sanitized metadata crosses IPC/socket,
- Project/conversation IDs are redacted from diagnostics unless needed internally for stable identity,
- cursor diagnostics use fingerprints,
- unknown response shapes fail closed,
- no failure path falls back to credential extraction.

A previous diagnostic path briefly exposed structural Project/conversation IDs when they appeared as field values. The sanitizer was corrected so reported values pass through the shared ID-redaction helper. No cookie/token/auth header was involved.

## Background helper lifecycle

A Go-owned pseudo-PTY can keep stock `terminal-browser` alive for the spike, but closing that PTY terminates the helper path. Stock `terminal-browser` still has no proven supported display-free/service lifecycle suitable for long-running production discovery.

Status:

```text
background helper: CONDITIONAL — PoC lifecycle workaround
```

~~Resolve this before Issue #6 can begin.~~

Treat this as production integration/hardening while implementing #6; it is not an unresolved fundamental decision from the spike.

## Failure behavior

Exercised failure cases fail closed:

- missing `terminal-browser`,
- stopped helper / broken bridge,
- logged-out browser partition,
- malformed/mismatched bridge responses,
- unknown cursor schema,
- broken cursor chain,
- cursor cycle,
- ambiguous series selection,
- incomplete scroll walk without terminal cursor.

A partial list must never be presented as complete.

## Results

| Phase | Result |
| --- | --- |
| 0. Baseline | CONDITIONAL |
| 1. Persistent authentication | PASS |
| 2. Background helper | CONDITIONAL — PoC workaround only |
| 3. Browser bridge | PASS |
| 4. Project discovery | PASS; rename/duplicate-name robustness NOT VERIFIED |
| 5. Session discovery | **PASS** — Project cursor pagination reached terminal cursor; 35 unique / 0 duplicates |
| ~~6. Chat / Work discrimination — CONDITIONAL (`async_source`, n=1)~~ | **NOT REQUIRED by selected product behavior** |
| 7. Stable identity and Open | PASS |
| 8. App-mode UX | PASS, with IME defect |
| 9. `Ctrl+]` semantics | PASS |
| 10. Failure behavior | PASS for exercised cases |

## Known limitations / implementation hardening

- All investigated `/backend-api/...` endpoints and schemas are undocumented and can change without notice.
- Session discovery is proven on one real account/Project; cross-account and version-window re-verification is still desirable before broad rollout.
- The List path depends on passive capture of the real frontend's cursor requests; a script-issued cursor fetch is unauthorized.
- `terminal-browser` has no proven supported display-free service lifecycle; the spike uses a pseudo-PTY owner.
- IME composition can submit prematurely in app-mode.
- v0.8.0 was tested while v0.8.1 was current at spike time.
- Project rename / duplicate-name behavior was not explicitly exercised.

These are implementation/compatibility concerns, not blockers to starting #6.

## Security boundary

The browser partition is the sole owner of ChatGPT authentication. Go and the socket protocol must never receive or log cookies, authorization headers, access tokens, refresh tokens, browser storage credentials, or complete request headers.

The socket is local and mode `0600`. Requests are size-limited and method-allowlisted. Raw endpoint responses remain browser-side. Failure never falls back to credential extraction.

## Issue #6 handoff

Issue #7 is complete. The implementation handoff is:

```text
authentication        → persistent terminal-browser partition
configured Project    → stable g-p-... project_id
List                  → Project-scoped cursor pagination via passive real-frontend capture
session key           → chatgpt:<conversation_id>
ordering              → local created_at DESC
pin                    → agentsctl-local only
Open / interaction    → official ChatGPT UI in terminal-browser app-mode
Chat vs Work type     → not required
Ctrl+]                → close browser view only; cloud state continues
```

Issue #6 can proceed without further spike work. Production hardening can happen as implementation details are resolved.
