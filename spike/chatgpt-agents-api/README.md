# ChatGPT ↔ official Agents API identity spike

## Purpose

This spike collects evidence for [Issue #7](https://github.com/tingtt/agentsctl/issues/7),
answering a narrower question than the sibling [`spike/chatgpt-browser`](../chatgpt-browser):

> Does a ChatGPT Web UI Work session appear as the same object in the official OpenAI
> Agents API (`GET /agents/sessions`)?

This is not the ChatGPT provider planned by Issue #6, and it does not change any
production code, provider interface, or the conclusions already recorded in
`spike/chatgpt-browser/README.md`. It uses only the documented, API-key-authenticated
Agents API — never ChatGPT's undocumented `/backend-api`, and never ChatGPT browser
cookies/tokens.

## Result: FAIL — separate namespaces

**A ChatGPT Work session created in the Web UI did not appear in `GET /agents/sessions`
for the same OpenAI account.** The official Managed Agents Sessions API and ChatGPT's
Work/Agent-mode feature are evidenced to be distinct products/namespaces for this
account, not the same session object under two names. See
[Phase B/C](#phase-bc-controlled-work-sample) for the experiment and
[Final report](#final-report) for the full breakdown.

The existing `spike/chatgpt-browser` conclusions are unchanged by this result — see that
spike's note on `async_source` below.

## Official API baseline (contract, from the SDK source and live responses)

The official docs page was not fetchable (`platform.openai.com` returned HTTP 403 to an
unauthenticated fetch), so the contract below was confirmed from two independent sources:
the generated Go SDK (`github.com/openai/openai-go`, `betaagentsession*.go`) and live,
read-only responses from this account.

Required header on every request: `OpenAI-Beta: agents=v1` (a plain `Authorization:
Bearer <key>` request without it is rejected with `invalid_beta`, confirmed live).

```text
GET /agents/sessions
  query: after, agent_id, limit, order (asc|desc, default desc)
  response: { object, data: [AgentSession], first_id, last_id, has_more }

GET /agents/sessions/{session_id}/items
  query: after, limit (1-100, default 20), order (asc|desc, default desc)
  response: { object, data: [AgentSessionItemUnion], first_id, last_id, has_more }

GET /agents/sessions/{session_id}/turns
  query: after, limit (1-100, default 20), order (asc|desc, default desc)
  response: { object, data: [Turn], first_id, last_id, has_more }
```

`AgentSession` documented fields (from the SDK's `AgentSession` struct):
`id`, `agent` (`id`, `instructions`, `model`, `multi_agent`, `name`, `reasoning`,
`service_tier`, `text`, `tools`), `created_at`, `environment` (`type`:
`none`/`openai_hosted`/`self_hosted`, plus sandbox/network/package/skill/`remote_url`/
`workspace_directory` fields — no ChatGPT- or Project-shaped field), `error`,
`last_active_at`, `metadata` (free-form, caller-supplied at creation), `required_actions`,
`status` (`idle`/`in_progress`/`requires_action`/`failed`), `usage`, `vault_ids`.

`Turn` documented fields: `id`, `agent_id`, `session_id`, `subagent_id`, `status`
(`queued`/`in_progress`/`waiting`/`completed`/`failed`/`cancelled`), `created_at`,
`started_at`, `completed_at`, `error`, `usage`.

**No documented field on `AgentSession`, `AgentSessionAgent`, or `EnvironmentUnion`
names a ChatGPT conversation ID, ChatGPT Project, or workspace/gizmo identity.** This was
checked directly against the SDK's generated struct definitions, not inferred from field
names — see [Phase I](#phase-i-project-association) for the empirical follow-up.

Pagination is ID-based cursor pagination (`after`/`limit`/`order`), not offset-based —
confirmed from the SDK doc comment ("Lists managed agent sessions using ID-based
pagination") and consistent with `first_id`/`last_id`/`has_more` in every live response.
This is a materially different, and simpler, contract than the undocumented
`offset`/`limit` pagination `spike/chatgpt-browser` had to reverse-engineer for
`/backend-api/conversations`.

## Authentication

`OPENAI_API_KEY` was supplied by the repository owner directly into this session for this
spike. It was verified to belong to the **same OpenAI account** the owner uses to log
into ChatGPT (self-reported, not independently provable from inside this harness). The
key is never printed, logged, or committed by this harness; it is read from an
environment variable at process start only. See [Credential handling](#credential-handling)
below for why it needed to be supplied this way and its resulting caveat.

## Harness

A small, read-only Go CLI (`main.go`, `client.go`, `logic.go`). It calls only
`GET /agents/sessions`, `GET /agents/sessions/{id}/items`, and
`GET /agents/sessions/{id}/turns` — no session creation, deletion, update, or event
submission, per this spike's explicit scope.

```bash
go test ./...
OPENAI_API_KEY=... go run . list [--limit N] [--order asc|desc] [--agent-id ID] [--state-dir DIR]
OPENAI_API_KEY=... go run . paginate [--limit N] [--order asc|desc]
OPENAI_API_KEY=... go run . diff --state-dir DIR
OPENAI_API_KEY=... go run . items --state-dir DIR --use candidate|newest|index:N [--marker STRING]
OPENAI_API_KEY=... go run . turns --state-dir DIR --use candidate|newest|index:N
```

Raw session/turn/conversation IDs never appear as CLI arguments (avoiding shell-history
exposure) and never appear in stdout: `list`/`diff`/`turns` print only a 12-hex-character
SHA-256 `fingerprint` of each ID, plus non-identifying fields (timestamps, status,
metadata *key names*, extra/undocumented top-level field *names*). `--state-dir` caches
one raw list response locally (outside the repo, never committed) purely so `items`/
`turns` can resolve a session by `candidate` (the one new session found by `diff`),
`newest`, or `index:N` — never by typing a raw ID.

## Phase A: baseline

```text
agents sessions:
  count_first_page=0
  has_more=false
  first_id_present=false
  last_id_present=false
```

Zero Managed Agents sessions existed for this account before any controlled Work sample
was created.

## Phase B/C: controlled Work sample

The repository owner created one new ChatGPT Work session in the Web UI and sent the
controlled marker `agentsctl-agents-api-probe-8a06f9c0` as its first message (marker
value only — not a secret, not linked to any personal data).

`list` was re-run immediately after, and again ~15s later with both `order=asc` and
`order=desc` and a larger `limit=100`, to rule out pagination-window and eventual-
consistency artifacts:

```text
before_count=0 after_count=0 new_count=0
controlled candidate: NOT VERIFIED (need exactly one new session)
```

Every re-check reproduced `count_first_page=0`. **No new Managed Agents session
appeared** for the same account that just created the Work session.

Before treating this as a real result rather than a credential/account mismatch, the
repository owner explicitly confirmed the supplied `OPENAI_API_KEY` belongs to the same
OpenAI account used to log into ChatGPT for this experiment.

Phases D and E (marker-in-items, turn/status inspection) require a candidate session,
which never existed — both are **N/A**, not merely unverified.

## Phase F/G: ID relationship and Open URL

**N/A.** No Agents API session ever appeared, so there is no `session.id` to compare
against a ChatGPT conversation ID, and no candidate to test against
`https://chatgpt.com/c/{id}`.

## Phase H: ordinary Chat negative control

**Skipped**, per the task's stop condition: once Work ↔ Agents API identity resolved to
FAIL (Outcome D — Work does not appear at all), running an ordinary-Chat sample would at
best reproduce the same "does not appear" result and could not change the answered
question. Recorded as **NOT VERIFIED** rather than a claimed PASS/FAIL, since it was not
directly tested.

## Phase I: Project association

**NOT AVAILABLE**, decided from the documented schema alone (not from field-name
guessing): `AgentSession`, its nested `agent` object, and `EnvironmentUnion` carry no
field that names or references a ChatGPT Project, workspace, or gizmo ID anywhere in the
generated SDK's struct definitions. `metadata` is a free-form, caller-supplied map (empty
unless explicitly set at session creation) — not a documented, stable Project-association
channel. This question is moot in any case, since Phase B/C already found that ChatGPT
Work sessions do not appear in this API at all.

## Pagination

The cursor-pagination *logic* (accumulate-with-dedup, detect duplicates across pages) is
covered by unit tests in `logic_test.go` using synthetic multi-page data. The *live*
two-page exercise (`paginate --limit 2`) could not be run end-to-end: with 0 sessions in
the account, `has_more` is `false` on page 1 and there is no page 2 to fetch.

```text
page1: count=0 has_more=false
page2: skipped (page1 already exhausted)
```

Result: **pagination logic PASS (unit-tested); live 2-page traversal NOT VERIFIED**
(no data to paginate over in this account).

## Credential handling

`OPENAI_API_KEY` was supplied by the repository owner directly as plaintext in the
conversation transcript (rather than staying only in a shell environment variable),
because this harness's execution environment does not persist shell state between
separate tool invocations — an `export` in one command is not visible to the next. The
key was written once to a local file outside the repository (this job's temporary
directory, not `/tmp`, not committed, cleaned up when the job ends) and read from there
into `OPENAI_API_KEY` inline in each command; it was never echoed, logged, or written
into this repository. **Because the raw key value is present in this conversation's
transcript, the repository owner was advised to rotate/revoke it after this spike**,
independent of this harness's own handling.

## Effect on `spike/chatgpt-browser`

The existing browser-backed conclusions are **unchanged**. Per the task's instruction,
the `async_source` strikethrough update is intentionally **not applied** — that update
was conditioned on a PASS here, and this spike's result is FAIL:

```text
async_source remains the leading Work discriminator for the browser-backed path.
Official Agents API Managed Agents sessions are evidenced to be a separate product/
namespace from ChatGPT Work for this account, not a supported replacement path.
```

## Final report

1. **Official API availability:** `GET /agents/sessions`: **PASS** (200, valid contract,
   confirmed against both the SDK source and live responses).
2. **Work controlled sample:** ChatGPT Work created: **yes**. New Agents session
   observed: **no**. Marker found in session items: **N/A** (no candidate session).
3. **Namespace decision:** ChatGPT Work ↔ Agents API: **FAIL** (Outcome D — Work does not
   appear in `GET /agents/sessions`).
4. **ID relationship:** Agents session ID == ChatGPT conversation ID: **NOT VERIFIED**
   (no Agents session ID exists to compare).
5. **Open mapping:** Agents session → ChatGPT `/c/{id}`: **NOT VERIFIED** (N/A, no
   session ID to test).
6. **Work status:** N/A — no session or turn ever existed to compare against ChatGPT
   Work UI state.
7. **Ordinary Chat negative:** ordinary Chat appears in Agents API: **NOT VERIFIED**
   (skipped per stop condition, once Work itself was decided FAIL).
8. **Project association:** configured ChatGPT Project determinable from Agents API:
   **NOT AVAILABLE** (decided from documented schema; also moot given #3).
9. **Pagination:** official cursor pagination: logic **PASS** (unit-tested); live 2-page
   exercise **NOT VERIFIED** (account has 0 sessions, nothing to page through).
10. **Architecture implication: C** — the official Agents API and ChatGPT Work are
    separate namespaces for this account; the existing browser-backed approach in
    `spike/chatgpt-browser` remains the relevant investigation path for Issue #7.
11. **Effect on previous blockers:** none of the four browser-backed blockers
    (Project-inclusive `/backend-api` pagination, `async_source` Work discriminator, Work
    execution status, background terminal-browser discovery) are resolved or made
    unnecessary by this result — see [Effect on `spike/chatgpt-browser`](#effect-on-spikechatgpt-browser).
12. **Files changed:** `spike/chatgpt-agents-api/{client.go,logic.go,main.go,logic_test.go,README.md}`
    (new sibling spike; no changes to `spike/chatgpt-browser`).
13. **Tests:** `go fmt ./...`, `go vet ./...`, `go build ./...`, `go test ./... -v -race`
    all pass (10 test cases covering fingerprinting, before/after diff, ambiguous diff,
    cursor accumulation/dedup, and marker matching).
14. **Commits:** see repository history on `spike/chatgpt-integrate-with-terminal-browser`.
15. **Push status:** pushed to the same remote branch; no PR opened, Issue #7/#6 not
    modified, per this task's explicit constraints.
