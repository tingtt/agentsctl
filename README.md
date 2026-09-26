# agentsctl

`agentsctl` is a Unix TUI for managing Claude Code background agents and Codex CLI sessions from one Agent View.

ChatGPT Project conversations can also be listed and opened with optional configuration.

## Installation

agentsctl supports macOS and Linux.

Install `claude` and/or `codex` first, then install agentsctl from a [GitHub Release](https://github.com/tingtt/agentsctl/releases) or with Go 1.25 or later. The command below installs the latest release and embeds its version, which enables update checks:

```sh
VERSION=$(go list -m -f '{{.Version}}' github.com/tingtt/agentsctl@latest)
go install \
  -ldflags="-X github.com/tingtt/agentsctl/internal/version.Version=${VERSION}" \
  github.com/tingtt/agentsctl/cmd/agentsctl@${VERSION}
```

Then start it from the directory you want to work in:

```sh
agentsctl
```

A GitHub Release binary, or one installed with the command above, checks for a newer release at startup and shows a notice above the prompt. With Go installed, type `/update` and press `Enter` to install the announced version with `go install` and restart; without Go, the notice links to the releases page. Local builds without an embedded version, such as `go build ./cmd/agentsctl`, stay at `dev` and skip the check.

## Managing Claude and Codex sessions

agentsctl combines Claude and Codex sessions into one list.

Type a prompt and press `Enter` to start a background session. Use `Shift+Tab` to switch between Claude and Codex.

With an empty prompt, select an existing session and press `Enter` to open it. For Claude, `Ctrl+]` returns to Agent View without stopping the session. A Codex session opens in the Codex TUI connected to Codex's shared app-server; quitting the TUI returns to Agent View and leaves the session and any running turn in place.

The session list can be scoped to the current directory, its descendants and Git worktrees, or all directories with `Ctrl+/`.

Directory groups initially show 10 sessions at a time with selectable `Show more` rows. Pinned sessions stay together and can be folded or restored as one group.

An opened Codex TUI inherits agentsctl's environment, so Codex's native external editor shortcut uses your `VISUAL` or `EDITOR`.

## Key bindings

| Key | Action |
| --- | --- |
| `Enter` | Dispatch a prompt, or open/expand the selected row when the prompt is empty |
| `Ctrl+]` | Detach from an opened Claude session or ChatGPT view and return to Agent View |
| `Shift+Tab` | Switch the prompt provider between Claude and Codex |
| `Option+Enter` / `Shift+Enter` | Insert a newline |
| `↑` / `↓` | Move between session/control rows, or move the multiline prompt cursor |
| `←` / `→` | Fold/expand a session group when the prompt is empty; otherwise move the prompt cursor |
| `{` / `}` | Move to the previous/next session group when the prompt is empty |
| `Ctrl+S` | Swap the prompt with the in-memory stash |
| `Ctrl+G` | Edit the prompt in Vim |
| `Ctrl+O` | Open the selected session |
| `Ctrl+T` | Pin or unpin the selected session |
| `Ctrl+/` | Cycle directory scope: current directory → current directory + descendants + worktrees → all directories |
| `Ctrl+R` | Rename the selected session |
| `Ctrl+X` | Stop or archive the selected session |
| `Ctrl+L` | Refresh sessions |
| `?` | Show help |
| `Esc` | Cancel, clear the prompt, or exit |

## ChatGPT Project

ChatGPT integration is optional and requires [zenbu-labs/terminal-browser](https://github.com/zenbu-labs/terminal-browser).

Add a Project ID to the nearest `.agentsctl.toml` at or above the directory where agentsctl starts:

```toml
[chatgpt]
project_id = "g-p-..."
```

Authenticate the dedicated browser partition:

```sh
terminal-browser open https://chatgpt.com --partition=agentsctl-chatgpt --app-mode
```

Configured Project conversations then appear alongside Claude and Codex sessions.

ChatGPT currently supports listing and opening existing conversations only. Interaction stays in the official ChatGPT UI, and `Ctrl+]` returns to Agent View without stopping or deleting the conversation.

## Documentation

See [DesignDoc.md](DesignDoc.md) for architecture and behavior, and [CONTRIBUTING.md](CONTRIBUTING.md) for development and release workflows.
