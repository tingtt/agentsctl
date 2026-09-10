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
- `projects`: sanitized Project ID, name, and an Open URL candidate.
- `conversations`: sanitized identity, title, times, archived state, Project association, selected discriminator candidates, response keys, and Open URL candidates, from the project-scoped endpoint.
- `globalConversations`: the same shape, but sourced from the global (unscoped) conversations endpoint and filtered client-side to the requested Project — needed because the project-scoped endpoint alone under-reports (see Phase 5).
- `conversationEvidence`: cross-conversation field/cardinality comparison for up to 20 IDs at once, used to hunt for a Chat/Work discriminator (Phase 6) without exposing conversation content, titles, or per-conversation identity mapping — allowlisted structural values (status/type/kind/mode/origin-style labels) are ID-redacted before being returned.
- `tasks`: sanitized evidence from the observed, undocumented `/backend-api/tasks` endpoint — investigated as a candidate Work-discovery path and rejected (see Phase 5); reports only counts and structural status-field names, never task content.
- `openURLProbe`: internally selects one conversation showing the Chat/Work discriminator candidate and one plain conversation (the chosen IDs never cross the socket), navigates each to both `/c/{id}` and `/g/{project_id}/c/{id}`, and reports readiness/login-redirect state plus an ID-redacted URL comparison (Phase 7).

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
```

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

**Method:** Attempted a foreground `terminal-browser` launch in the current terminal; app-mode requires the same graphics path.

**Observed:** The terminal was rejected as unable to show images. Transcript, composer, messages, Work UI, files, approvals, rich content, and popup flows were not exercised.

**Result: NOT VERIFIED**

**Implication:** UI delegation cannot be accepted from this environment. A kitty-capable terminal is mandatory for the remaining experiment.

### Phase 9: return with Ctrl+]

**Hypothesis:** A preload handler can close only the browser view while cloud execution continues.

**Method:** Added an opt-in capture handler using the documented `globalThis.terminalBrowser.quit()` API when `AGENTSCTL_CHATGPT_CLOSE_KEY=1`.

**Observed:** Source and code path exist, but no visible authenticated Work execution was available for an end-to-end test.

**Result: NOT VERIFIED**

**Implication:** View-close mechanics are plausible; cloud Work continuity must be observed before acceptance.

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
| 5. Session discovery | PASS, method corrected — global `/backend-api/conversations` filtered by Project required; project-scoped endpoint alone under-reports (5 of 17) |
| 6. Chat / Work discrimination | CONDITIONAL — B. `async_source` field presence is a plausible undocumented discriminator (n=1 Work sample) |
| 7. Stable identity and Open | PASS — bare `/c/{conversation_id}` is sufficient; ChatGPT normalizes to the full Project-slug URL itself |
| 8. App-mode UX | NOT VERIFIED — requires a kitty-capable terminal, not available in the sandboxed harness |
| 9. Ctrl+] semantics | NOT VERIFIED — same blocker as Phase 8 |
| 10. Failure behavior | PASS for exercised cases |

## Capability matrix

`Proven` means observed in this run. `Prepared` means the PoC has a guarded path that could not be exercised due to missing authentication.

| Capability | Programmatic | Browser UI | Stability | Notes |
| --- | --- | --- | --- | --- |
| List | Proven, method-corrected (Project resolution + 17-conversation Project-filtered global list; project-scoped endpoint alone under-reports) | N/A | Undocumented / unstable | Two independent endpoints needed; `/backend-api/tasks` investigated and rejected as unrelated |
| Open | Proven | N/A (delegates to browser UI) | Undocumented navigation behavior, but consistent across 4 probes | Bare `/c/{id}` normalizes correctly for both Chat and a Work-marked sample; no login redirect |
| Read transcript | Out of scope | Not verified | Unknown | UI could not render in this sandboxed terminal |
| Send message | Out of scope | Not verified | Unknown | No test message sent |
| Continue Chat | Out of scope | Not verified | Unknown | Requires a kitty-capable terminal |
| Continue Work | Out of scope | Not verified | Unknown | Requires a kitty-capable terminal |
| Observe Work state | Conditional — B (undocumented) | Not verified | Fragile | `messages.[].metadata.async_source` presence is a plausible marker (n=1 sample); top-level `async_status` stayed null and is likely transient, not durable |
| Rename | Out of scope | Not verified | Unknown | No destructive or mutating probe |
| Archive/delete | Out of scope | Not verified | Unknown | No destructive probe |

## Known limitations

- Stock `terminal-browser` has no proven display-free service lifecycle. The PTY is a required liveness owner.
- The sandboxed harness's terminal cannot display terminal-browser graphics; Phases 8 and 9 (app-mode UX, `Ctrl+]`) still require a kitty-capable terminal and have not been exercised at all.
- Authentication, Project discovery, and session discovery (Phases 1, 4, 5) are proven against the human's real account and real Project via a human-operated terminal, after one manual login. Session discovery required correcting the method mid-run: the project-scoped conversations endpoint returns an incomplete list (5 of 17), so a production adapter needs the global, Project-filtered endpoint too.
- Chat/Work discrimination (Phase 6) has moved from "no evidence" to "one undocumented candidate field (`async_source`) confirmed on a single human-created Work sample." It is not yet confirmed absent from ordinary tool-using Chats, and is not a documented, stable API guarantee.
- **Mid-run correction:** the field-value reporting added for Phase 6 initially printed unredacted `g-p-...` Project IDs and full conversation/turn UUIDs when they appeared as a structural field's *value* (e.g. `conversation_template_id`, `working_turn_id`) rather than as a key. The existing ID-redaction (already applied to `observedBackendPaths` and the Open-URL diagnostic) was not applied to this path. This was caught during the same session, before any further extraction, and fixed by routing all reported values through the shared redaction helper while still computing distinct-value counts from the raw (unredacted) values, so per-item uniqueness signals aren't lost. No cookie, token, or authorization header was ever involved; the exposed values were structural identifiers already visible to the operator from ChatGPT's own URLs. Any adapter built on this pattern must route every value that might contain an ID through the same redaction before logging.
- All investigated `/backend-api/...` endpoints and schemas are observed, undocumented, and unstable.
- The preload sanitizer deliberately rejects unknown shapes; ChatGPT changes will disable discovery until the adapter is reviewed.
- v0.8.0 was tested while v0.8.1 was current.

## Security boundary

The browser partition is the sole owner of ChatGPT authentication. The Go process and socket protocol must never receive or log cookies, authorization headers, access tokens, refresh tokens, browser storage credentials, or complete request headers. Raw endpoint responses remain inside the preload; only explicitly selected Project/conversation metadata can cross IPC.

The socket is local and changed to mode `0600`. Requests are size-limited and method allowlisted. Fetches are same-origin, credential-preserving browser calls to fixed path shapes. Failure never falls back to credential extraction.

Structural field values (status/type/kind/mode/origin-style labels used for Chat/Work discrimination evidence) are permitted to cross the bridge, but only after ID-redaction — see the Phase 6 mid-run correction in Known limitations. Conversation content, titles, and per-conversation identity mapping are never logged, by design of the aggregate-only evidence comparison.

## Recommendation

**Current decision: CONDITIONAL GO.**

The bridge mechanism, persistent authentication, Project discovery, session discovery, and canonical Open URL are now proven against a real account and a real, human-created Work sample. What remains are the limited blockers CONDITIONAL GO anticipates: the background helper lifecycle is a PTY workaround, not a production lifecycle; the Chat/Work discriminator (`async_source`) is undocumented and confirmed on only one sample; and app-mode UX / `Ctrl+]` semantics are architecturally plausible but unverified because this harness has no kitty-capable terminal.

```text
#6 implementation before blocker resolution: no
```

Before starting Issue #6:

1. Corroborate the `async_source`-presence discriminator against more than one Work sample, and confirm it does not also appear on ordinary tool-using Chats (browsing, code interpreter, etc., which already share several marker field names). Do not ship a heuristic confirmed on n=1.
2. Run Phases 8–9 (app-mode UX, `Ctrl+]` view-close-without-stopping-Work semantics) from a kitty-capable terminal — the only phases this sandboxed harness structurally cannot exercise.
3. Obtain a supported `terminal-browser` no-render/service lifecycle, or an explicit upstream commitment, suitable for production; the current pseudo-PTY is a PoC workaround only (Phase 2).
4. Build the production session-discovery adapter against the global `/backend-api/conversations` endpoint filtered by Project association (Phase 5's corrected method), not the project-scoped endpoint alone.
5. Re-run against the selected/pinned `terminal-browser` version (v0.8.0 tested vs. v0.8.1 current at spike time) and document its compatibility window.

None of these require abandoning the browser-backed direction — they are scoped hardening and verification steps, consistent with CONDITIONAL GO. Do not add a production ChatGPT provider or change shared provider/session contracts from this spike alone.

