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
| `↑` / `↓` | Move the session selection. |
| `Ctrl+A` | Toggle current-directory and all-directory session scopes. The initial scope is the directory where `agentsctl` was started. |
| `Ctrl+R` | Rename the selected session. Edit the current name inline with `←` / `→`, `Home`, `End`, `Backspace`, and `Delete`; use `Enter` to save or `Esc` to cancel. |
| `Ctrl+X` | Stop an active managed session. For an inactive session, press twice to confirm and archive it. |
| `Ctrl+L` | Refresh the active session catalog and runtime state. |
| `Ctrl+]` | Detach to the overview without stopping the underlying session. |
| `Esc` | Cancel rename/archive confirmation, or exit the overview. Background sessions continue. |

Archive removes a session from the MVP catalog and is a one-way TUI operation.

The prompt stash stores text only. It is shared across providers, folders, and selected sessions, and is discarded when `agentsctl` exits. Rename and archive confirmation keep both the composer and stash unchanged.

Claude sessions use Claude's native background supervisor. Detaching translates `Ctrl+]` into Claude's native `Ctrl+Z` detach sequence and waits for `claude attach` to exit; timeout does not kill the attachment or background agent. Claude archive is only a local visibility overlay: it never deletes the session, transcript, or worktree.

Codex conversations and archive state come from the Codex app-server. Only Codex gets an `agentsctl` supervisor: it owns a PTY for managed interactive CLI processes so a TUI restart can reattach while the daemon remains alive. The daemon resolves `codex` from its own effective `PATH` and rejects incompatible protocol/build generations. External or ambiguous writers are fail-closed and cannot be attached or stopped. A new run remains a diagnostic `Starting` row until exactly one new app-server thread is proven; zero or multiple candidates are never guessed.

`Stop`, `Detach`, and `Archive` are separate operations. Detach only disconnects the attachment client. Stop signals only a process currently owned by the `agentsctl` supervisor (or asks Claude's native supervisor). Archive never implies stop and is disabled for active sessions.
