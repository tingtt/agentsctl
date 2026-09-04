# agentsctl

`agentsctl` is a Unix TUI that presents Claude Code background agents and Codex CLI sessions in one Agent View. It normalizes list, dispatch, attach, detach, stop, rename, and archive as capabilities while keeping each provider's lifecycle intact.

## Requirements

- macOS or Linux
- Go 1.25+ to build from source
- `claude` and/or `codex` on `PATH`; either provider may be unavailable

```sh
go build ./cmd/agentsctl
./agentsctl
```

## Keys

| Key | Action |
| --- | --- |
| `Shift+Tab` | Toggle the composer between Claude and Codex without clearing the prompt. |
| `Enter` | Dispatch the composer prompt in the background, or attach the selected session when the composer is empty. |
| `Ctrl+S` | Swap the composer text with one shared in-memory stash slot. While attached, forward `Ctrl+S` to the child instead. |
| `Ctrl+O` | Attach the selected session. |
| `Ctrl+T` | Pin or unpin the selected session. Pin state is persisted by `agentsctl`. |
| `↑` / `↓` | Move the session selection. |
| `Ctrl+G` | Cycle the session-list directory scope: `cwd` → `cwd/**` → `all` → `cwd`. The starting point is the directory `agentsctl` was started in; the current scope is shown in the header. |
| `Ctrl+R` | Rename the selected session. Edit the current name inline with `←` / `→`, `Home`, `End`, `Backspace`, and `Delete`; use `Enter` to save or `Esc` to cancel. |
| `Ctrl+X` | Stop an active managed session. For an inactive session, press twice to confirm and archive it. The confirmation ("Press Ctrl+X again to archive") appears on that session's own row, not as a separate message. |
| `Ctrl+L` | Refresh the active session catalog and runtime state. |
| `Ctrl+]` | Detach to the overview without stopping the underlying session. |
| `Esc` | Cancel rename/archive confirmation, or exit the overview. Background sessions continue. |

Archive removes a session from the MVP catalog and is a one-way TUI operation.

The overview has `Pinned` and `Other` groups. Each group is ordered by native session creation time, newest first, so activity and status refreshes do not move sessions. Selection, rendering, and viewport calculation use this same order.

The composer supports `←` / `→`, `Home`, `End`, `Backspace`, and `Delete` with a visible cursor. The prompt stash stores text only. It is shared across providers, directory scopes, and selected sessions, and is discarded when `agentsctl` exits. Restoring a stashed prompt places the cursor at its end. Rename and archive confirmation keep both the composer and stash unchanged.

### Directory scope

`Ctrl+G` cycles the session list through three directory scopes, relative to the directory `agentsctl` was started in (its logical, `filepath.Clean`-ed path; symlinks are never resolved):

- `cwd` — only sessions whose CWD is exactly that directory.
- `cwd/**` — that directory itself, plus any descendant subdirectory (an inclusive recursive subtree). A sibling directory that merely shares a path prefix (e.g. `project` vs. `project-other`) is not included — the comparison is path-boundary-aware, not a string prefix match.
- `all` — no directory filter.

The current scope is shown in the header (`agentsctl · cwd`, `agentsctl · cwd/**`, or `agentsctl · all`); switching scopes does not produce a separate notification.

### Notifications

The composer-top notification area is reserved for errors only (a rejected action, an empty name, an unavailable capability). There is no generic non-error notification: an operation whose result is already visible elsewhere in the UI gets none. Dispatching a session, cycling the CWD depth (`Ctrl+/`), and cancelling a rename or an archive confirmation (`Esc`) are all feedback through the UI itself — a new row appearing, every row's CWD column changing, the inline editor closing back to the row, the row's red confirmation disappearing — not through a message.

A notice scoped to a single session (currently only the `Ctrl+X` archive confirmation) renders on that session's own row, immediately before the provider/cwd block, right-aligned. Width is allocated in strict priority order under pressure: the provider/CWD block is never shrunk for a notice, the notice itself is clipped or dropped next, and the title is squeezed first of all — only when the terminal is too narrow even for provider+CWD does CWD itself give way (left-truncated). Row notices carry a severity: alert (red) for the archive confirmation, with info (cyan) reserved for future use. The confirmation always tracks the session by its key, so it follows the row through reordering (pin, refresh) and disappears when selection moves elsewhere or the action is cancelled with `Esc`.

Claude sessions use Claude's native background supervisor. Detaching sends Claude's own native detach byte (Ctrl+Z) to the `claude attach` client, the same key the CLI documents and honors by unwinding its own raw terminal mode and exiting on its own; only if the client does not exit does `agentsctl` fall back to signaling that client's process group. Either way, only the `claude attach` client owned by `agentsctl` ends — the native daemon and background session remain alive and can be attached again. Completed, non-archived Claude sessions remain attachable. Claude archive is only a local visibility overlay: it never deletes the session, transcript, or worktree.

Claude rename is also an **agentsctl-local** overlay, not a native Claude operation: the installed Claude CLI has no headless/native way to rename an existing background session in place (`claude --bg --resume <id> --name <name>` was verified to always fork a new session — under a different ID — rather than mutate the original's saved options, for any session state, active or stopped, and even given the full session ID rather than the short one). Claude's own auto-naming does update session-owned state, but by appending an undocumented `agent-name` record to the session's live, concurrently-written JSONL transcript — not something `agentsctl` writes to. The rename is instead stored keyed by provider and session ID and applied on top of the native name for display; it survives `agentsctl` restarts but is invisible to Claude's own UI/CLI and is available for any non-archived session regardless of whether it is active, since it never stops or otherwise touches the session.

Codex conversations, creation time, and archive state come from the Codex app-server. Only Codex gets an `agentsctl` supervisor: it owns a PTY for managed interactive CLI processes so a TUI restart can reattach while the daemon remains alive. On attach, the client synchronizes terminal size. A newly attaching client gets no scrollback replay, so on reattach with an unchanged size the supervisor briefly bounces the PTY to a harmless alternate size and back — two genuine, kernel-delivered `SIGWINCH`-inducing size changes to its owned PTY process group — so Codex fully repaints; a signal raised without an underlying size change was verified against the installed CLI to be silently ignored, since Codex re-reads the size on signal and only repaints when it actually differs. The daemon resolves `codex` from its own effective `PATH` and rejects incompatible protocol/build generations. External or ambiguous writers are fail-closed and cannot be attached or stopped. A new run remains a diagnostic `Starting` row until exactly one new app-server thread is proven; zero or multiple candidates are never guessed.

A run that reaches a terminal state (failed, stale, or stopped) without ever being proven to an app-server thread is shown as an `Unbound run` row. Its key is `agentsctl`'s own local run ID, never a Codex thread ID, so archiving it (`Ctrl+X` twice) is a local cleanup of that run record — not the native `thread/archive` call a real Codex thread's archive uses; that local run ID is never sent to the app-server, in any state. It is not stoppable, and whatever error caused the run to fail is kept only as diagnostic information; it never blocks archiving the row. A still-running or starting unbound run cannot be archived at all — the provider rejects it with an error rather than either deleting it locally or forwarding its ID to the app-server — and the local cleanup itself re-confirms the run is still unbound and terminal at the moment it actually deletes it, not just when the archive was requested.

`Stop`, `Detach`, and `Archive` are separate operations. Detach only disconnects the attachment client. Stop signals only a process currently owned by the `agentsctl` supervisor (or asks Claude's native supervisor). Archive never implies stop and is disabled for active sessions.
