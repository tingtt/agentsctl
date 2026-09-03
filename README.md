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
| `Ctrl+A` | Toggle current-directory and all-directory session scopes. The initial scope is the directory where `agentsctl` was started. |
| `Ctrl+R` | Rename the selected session. Edit the current name inline with `←` / `→`, `Home`, `End`, `Backspace`, and `Delete`; use `Enter` to save or `Esc` to cancel. |
| `Ctrl+X` | Stop an active managed session. For an inactive session, press twice to confirm and archive it. |
| `Ctrl+L` | Refresh the active session catalog and runtime state. |
| `Ctrl+]` | Detach to the overview without stopping the underlying session. |
| `Esc` | Cancel rename/archive confirmation, or exit the overview. Background sessions continue. |

Archive removes a session from the MVP catalog and is a one-way TUI operation.

The overview has `Pinned` and `Other` groups. Each group is ordered by native session creation time, newest first, so activity and status refreshes do not move sessions. Selection, rendering, and viewport calculation use this same order.

The composer supports `←` / `→`, `Home`, `End`, `Backspace`, and `Delete` with a visible cursor. The prompt stash stores text only. It is shared across providers, folders, and selected sessions, and is discarded when `agentsctl` exits. Restoring a stashed prompt places the cursor at its end. Rename and archive confirmation keep both the composer and stash unchanged.

Claude sessions use Claude's native background supervisor. Detaching sends Claude's own native detach byte (Ctrl+Z) to the `claude attach` client, the same key the CLI documents and honors by unwinding its own raw terminal mode and exiting on its own; only if the client does not exit does `agentsctl` fall back to signaling that client's process group. Either way, only the `claude attach` client owned by `agentsctl` ends — the native daemon and background session remain alive and can be attached again. Completed, non-archived Claude sessions remain attachable. Claude archive is only a local visibility overlay: it never deletes the session, transcript, or worktree.

Codex conversations, creation time, and archive state come from the Codex app-server. Only Codex gets an `agentsctl` supervisor: it owns a PTY for managed interactive CLI processes so a TUI restart can reattach while the daemon remains alive. On attach, the client synchronizes terminal size. A newly attaching client gets no scrollback replay, so on reattach with an unchanged size the supervisor briefly bounces the PTY to a harmless alternate size and back — two genuine, kernel-delivered `SIGWINCH`-inducing size changes to its owned PTY process group — so Codex fully repaints; a signal raised without an underlying size change was verified against the installed CLI to be silently ignored, since Codex re-reads the size on signal and only repaints when it actually differs. The daemon resolves `codex` from its own effective `PATH` and rejects incompatible protocol/build generations. External or ambiguous writers are fail-closed and cannot be attached or stopped. A new run remains a diagnostic `Starting` row until exactly one new app-server thread is proven; zero or multiple candidates are never guessed.

`Stop`, `Detach`, and `Archive` are separate operations. Detach only disconnects the attachment client. Stop signals only a process currently owned by the `agentsctl` supervisor (or asks Claude's native supervisor). Archive never implies stop and is disabled for active sessions.
