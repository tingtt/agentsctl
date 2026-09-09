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
| `Option+Enter` / `Shift+Enter` | Insert a newline into the composer prompt at the cursor, without dispatching. |
| `Ctrl+S` | Swap the composer text with one shared in-memory stash slot. While attached, forward `Ctrl+S` to the child instead. |
| `Ctrl+G` | Edit the composer prompt in Vim. Saving and exiting updates the composer without dispatching; exiting without saving preserves it. |
| `Ctrl+O` | Attach the selected session. |
| `Ctrl+T` | Pin or unpin the selected session. Pin state is persisted by `agentsctl`. |
| `↑` / `↓` | Move the session selection, or -- while the composer prompt spans more than one line -- move the cursor to the line above/below within the prompt instead (see Multiline editing). |
| `Ctrl+/` | Cycle the session-list directory scope: `same directory` → `descendants + worktree directories` → `all directories` → `same directory`. The starting point is the directory `agentsctl` was started in; the current scope is shown in the header. |
| `Ctrl+R` | Rename the selected session. Edit the current name inline with `←` / `→`, `Home`, `End`, `Backspace`, and `Delete`; use `Enter` to save or `Esc` to cancel. |
| `Ctrl+X` | Stop an active managed session. For an inactive session, press twice to confirm and archive it. The confirmation ("Press Ctrl+X again to archive") appears on that session's own row, not as a separate message. |
| `Ctrl+L` | Refresh the active session catalog and runtime state. |
| `Ctrl+]` | Detach to the overview without stopping the underlying session. |
| `?` | Show the help view, only when the composer prompt is empty and help isn't already shown (otherwise `?` is a plain prompt character). |
| `Esc` | Hide the help view if it's showing; otherwise clear a non-empty composer prompt; otherwise cancel a rename/archive confirmation, or exit the overview. Background sessions continue. |

Archive removes a session from the MVP catalog and is a one-way TUI operation.

### Session list grouping

Pinned sessions always form a single `Pinned` group, regardless of how many directories the current scope spans. Unpinned sessions form one `Recently created` group when every visible session shares the scope's one directory, or one group per distinct directory -- each headed by that directory's path -- once the scope spans more than one. A session row shows its own directory only when it's Pinned and the scope spans more than one directory; otherwise the group heading (or the single-directory scope itself) already says it. Within a group, sessions are ordered by native creation time, newest first, so activity and status refreshes do not move rows. Selection, rendering, and viewport calculation follow this same order.

### Composer

```text
──────────────────────────────────────────────────────────────── <cwd> ─
❯ prompt
──────────────────────────────────────────────────────────────────────
  claude (shift+tab to cycle) · ctrl+g to edit in vim · ctrl+x to stop · ? to show help · esc to exit
  claude  70% (reset at 07:10 AM) /  20% (reset at Sun 05:00 AM) · codex   0% (reset at 10:00 AM) / 100% (reset at Sat 11:00 PM)
```

`<cwd>` is the selected session's own directory (see Composer directory context below), and is what a newly dispatched prompt's session is created in. Below the prompt, a contextual line shows the current provider, its shortcut to cycle providers, the selected session's own stop/archive shortcut, and the current empty/non-empty-prompt hints (`? to show help` / `esc to exit`, or `esc to clear`) -- never a fixed, always-on shortcut list. Below that, a usage line shows each provider's 5-hour and weekly rate-limit utilization with its reset time, when that provider reports it; a provider that doesn't report usage (or reports none this refresh) is simply omitted, never shown as 0%. Usage is fetched in the background and never delays the catalog appearing or redrawing -- each provider's reading is applied, and the screen redrawn, the moment it arrives, independently of the others and without waiting for a key press. Both `claude` and `codex` always get their own segment on this line; one that hasn't reported anything yet, or whose last successful reading is more than 5 minutes old, shows `?%` in place of a percentage (`claude   ?% /   ?%`) rather than a stale or absent number.

Pressing `?` on an empty prompt replaces the contextual/usage lines with a categorized help view (`manage sessions` / `prompt` / `help`) instead. Help stays open while typing; only `Esc` closes it, without touching the prompt.

The composer supports `←` / `→`, `Home`, `End`, `Backspace`, and `Delete` with a visible cursor. It supports multiline prompts: `Option+Enter` or `Shift+Enter` inserts a newline at the cursor instead of dispatching, and each embedded newline renders as its own row, indented to align under the `❯ ` prefix; `←` / `→` move across a newline like any other character, and `Home` / `End` still jump to the start/end of the whole prompt (not just the current line). Only plain `Enter` dispatches (or, on an empty composer, attaches). `Ctrl+G` opens the complete prompt in foreground Vim; `:wq` returns the saved text to the composer, while `:q!` leaves the prior composer state intact. Returning from Vim never dispatches or starts a session—the normal submit action is still required. The prompt stash and the `Shift+Tab` provider toggle preserve multiline content, including embedded newlines, exactly as typed. The prompt stash stores text only. It is shared across providers, directory scopes, and selected sessions, and is discarded when `agentsctl` exits. Restoring a stashed prompt places the cursor at its end. Rename and archive confirmation keep both the composer and stash unchanged.

Whether `Shift+Enter` is distinguishable from plain `Enter` depends on the terminal: `agentsctl` recognizes it when the terminal sends a bare line feed (`\n`) for `Shift+Enter` as opposed to a carriage return (`\r`) for plain `Enter` (confirmed against a real macOS terminal via a raw-byte probe). A terminal that instead sends the identical byte for both cannot be distinguished at the application level. `Option+Enter` works wherever the terminal sends the classic "meta sends escape" convention (`ESC` followed by the Enter byte) for the Option modifier, which is how it was confirmed.

#### Multiline editing

Once the prompt spans more than one line, `↑` / `↓` move the cursor between logical lines instead of the session selection -- the column is preserved across a run of vertical moves the same way most text editors do (moving through a shorter line and back onto a longer one restores the original column). A single-line (or empty) prompt keeps `↑` / `↓` as session-list navigation.

#### Composer directory context

The composer's `<cwd>` -- and the directory a newly dispatched prompt runs in -- follows the currently selected session, not the directory `agentsctl` was started in. Moving selection to a session in another directory updates both together. Only when no session is selectable at all (an empty catalog) does the composer fall back to the startup directory, so a first prompt can still be dispatched from nothing.

### Directory scope

`Ctrl+/` cycles the session list through three directory scopes, relative to the directory `agentsctl` was started in (its logical, `filepath.Clean`-ed path; symlinks are never resolved):

- `same directory` — only sessions whose CWD is exactly that directory.
- `descendants + worktree directories` — that directory itself, any descendant subdirectory (an inclusive recursive subtree), and the working directories of any `git worktree`s belonging to the same repository (each also included together with its own descendants). A sibling directory that merely shares a path prefix (e.g. `project` vs. `project-other`) is not included — the comparison is path-boundary-aware, not a string prefix match. Worktree directories are discovered via `git worktree list --porcelain`; when that isn't possible (not a git repository, `git` unavailable), the scope simply falls back to the directory's own subtree.
- `all directories` — no directory filter.

The current scope is shown in the header (`agentsctl · same`, `agentsctl · descendants`, or `agentsctl · all`); switching scopes does not produce a separate notification.

### Notifications

The composer-top notification area is reserved for errors only (a rejected action, an empty name, an unavailable capability). There is no generic non-error notification: an operation whose result is already visible elsewhere in the UI gets none. Dispatching a session and cancelling a rename or an archive confirmation (`Esc`) are all feedback through the UI itself — a new row appearing, the inline editor closing back to the row, the row's red confirmation disappearing — not through a message.

A notice scoped to a single session (currently only the `Ctrl+X` archive confirmation) renders on that session's own row, immediately before the CWD/provider block (a Pinned row across more than one directory renders `title/notice -> cwd -> provider`, in that order), right-aligned. Width is allocated in strict priority order under pressure: the provider/CWD block is never shrunk for a notice, the notice itself is clipped or dropped next, and the title is squeezed first of all — only when the terminal is too narrow even for provider+CWD does CWD itself give way (left-truncated). Row notices carry a severity: alert (red) for the archive confirmation, with info (cyan) reserved for future use. The confirmation always tracks the session by its key, so it follows the row through reordering (pin, refresh) and disappears when selection moves elsewhere or the action is cancelled with `Esc`.

Claude sessions use Claude's native background supervisor. Detaching sends Claude's own native detach byte (Ctrl+Z) to the `claude attach` client, the same key the CLI documents and honors by unwinding its own raw terminal mode and exiting on its own; only if the client does not exit does `agentsctl` fall back to signaling that client's process group. Either way, only the `claude attach` client owned by `agentsctl` ends — the native daemon and background session remain alive and can be attached again. Completed, non-archived Claude sessions remain attachable. Claude archive is only a local visibility overlay: it never deletes the session, transcript, or worktree.

Claude rename is a **native** Claude operation: `agentsctl` renames the very same background session Claude itself tracks, so the new name shows up in `claude agents --json --all` and Claude's own Agent View too, not just in `agentsctl`. The session's ID, its background execution, and its lifecycle are all left untouched — renaming a session that is actively working does not interrupt it, and does not fork a new session (`claude --bg --resume <id> --name <name>` was verified to always fork a new session — under a different ID — rather than mutate the original's saved options, for any session state, active or stopped, which is why `agentsctl` does not use it). It works the same way for a completed session as for one still running, and is available for any non-archived session regardless of whether it is active. A name may contain spaces or non-ASCII text (Japanese, for example) but never a control character (including newlines) — those are rejected outright, since Claude's rename command reads its argument as a single line of terminal input. A leftover display-name override from before `agentsctl` supported native rename is only ever shown for a session Claude itself reports no name for, and is cleared automatically the next time that session is renamed.

Codex conversations, creation time, and archive state come from the Codex app-server. Only Codex gets an `agentsctl` supervisor: it owns a PTY for managed interactive CLI processes so a TUI restart can reattach while the daemon remains alive. On attach, the client synchronizes terminal size. A newly attaching client gets no scrollback replay, so on reattach with an unchanged size the supervisor briefly bounces the PTY to a harmless alternate size and back — two genuine, kernel-delivered `SIGWINCH`-inducing size changes to its owned PTY process group — so Codex fully repaints; a signal raised without an underlying size change was verified against the installed CLI to be silently ignored, since Codex re-reads the size on signal and only repaints when it actually differs. The daemon resolves `codex` from its own effective `PATH` and rejects incompatible protocol/build generations. External or ambiguous writers are fail-closed and cannot be attached or stopped. A new run remains a diagnostic `Starting` row until exactly one new app-server thread is proven; zero or multiple candidates are never guessed.

Codex's 5h/weekly usage (shown in the composer's usage line) comes from the same app-server connection, via its `account/rateLimits/read` method. Each returned window is classified into 5h/weekly by its own `windowDurationMins` (300/10080 respectively), never by whether it arrived as `primary` or `secondary` -- the app-server has been observed to place either window in either slot. A window with an unrecognized or missing duration is left unclassified rather than guessed.

Claude has no equivalent on-demand RPC, so its usage comes from a dedicated, `agentsctl`-owned interactive Claude session (a "usage probe") that `agentsctl` itself creates and never an existing user session. The probe's own dedicated Claude settings (never `~/.claude/settings.json`) point its `statusLine` at a small `agentsctl` subcommand that persists whatever rate-limit snapshot Claude Code hands it. The probe session's own identity is remembered locally so `agentsctl` reuses the same one across restarts rather than creating a new one every launch (including reusing a still-fresh persisted snapshot immediately on startup, without launching a process at all), and that same identity is what excludes the probe from the normal session list, catalog, and every session action -- never by CWD or name alone. Its dedicated directory is one Claude Code has never seen, so the very first probe launch ever answers Claude Code's own workspace-trust prompt once; Claude Code itself remembers that directory as trusted from then on, so no later launch repeats it. Usage reads are cached with a TTL and refreshed through the probe only when stale (never once per reload, and never on the Agent View's own render path -- Codex's usage, and the session catalog itself, are never held up waiting on Claude's); a refresh failure falls back to the last known snapshot rather than dropping the usage line, and Claude is only omitted from it if no snapshot has ever been obtained.

A run that reaches a terminal state (failed, stale, or stopped) without ever being proven to an app-server thread is shown as an `Unbound run` row. Its key is `agentsctl`'s own local run ID, never a Codex thread ID, so archiving it (`Ctrl+X` twice) is a local cleanup of that run record — not the native `thread/archive` call a real Codex thread's archive uses; that local run ID is never sent to the app-server, in any state. It is not stoppable, and whatever error caused the run to fail is kept only as diagnostic information; it never blocks archiving the row. A still-running or starting unbound run cannot be archived at all — the provider rejects it with an error rather than either deleting it locally or forwarding its ID to the app-server — and the local cleanup itself re-confirms the run is still unbound and terminal at the moment it actually deletes it, not just when the archive was requested.

`Stop`, `Detach`, and `Archive` are separate operations. Detach only disconnects the attachment client. Stop signals only a process currently owned by the `agentsctl` supervisor (or asks Claude's native supervisor). Archive never implies stop and is disabled for active sessions.
