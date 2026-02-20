# Dowse

[![Build](https://github.com/dreammify/dowse/actions/workflows/build.yml/badge.svg)](https://github.com/dreammify/dowse/actions/workflows/build.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/dreammify/dowse)](https://goreportcard.com/report/github.com/dreammify/dowse)
[![Go Reference](https://pkg.go.dev/badge/github.com/dreammify/dowse.svg)](https://pkg.go.dev/github.com/dreammify/dowse)

Dowse is a CLI tool that bridges Language Server Protocol (LSP) servers and AI agents. It runs a background daemon that manages LSP connections, file watchers, and diagnostics caching, then exposes a simple command-line interface that agents call on demand.

## Quick Start

### Install

```bash
go install github.com/dreammify/dowse/cmd/dowse@latest
```

Or build from source:

```bash
go build -o dowse ./cmd/dowse
```

### Configure

Create `~/.dowse/config.toml`:

```toml
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]

[[lsp]]
extensions = [".kt", ".kts"]
command = ["kotlin-lsp"]
```

You can also place a `.dowse.toml` in a project's git root to override settings per workspace. Workspace entries override global entries for matching extensions. In addition, placing a `.dowse.toml` file in a location means dowse will only traverse up to that location to run the LSP. So for example if you have a monorepo with multiple projects, adding a `.dowse.toml` per project means that the LSP will be run in that location for that project rather than the git root (which is the behavior if you don't use `.dowse.toml`)

### Run

```bash
# Start the daemon (runs in the background)
dowse start

# Get diagnostics for a file
dowse diagnostics /path/to/project/src/main.go

# Stop the daemon
dowse stop
```

The first diagnostics query for a given workspace + language may be slow as Dowse lazily starts the LSP server and waits for it to initialize.

## Usage

### `dowse start [--foreground]`

Start the daemon. Idempotent -- running it again when the daemon is already active is a no-op.

Use `--foreground` to run in the foreground with logs to stderr (useful for debugging).

### `dowse stop`

Shut down the daemon and all active LSP sessions.

### `dowse diagnostics [flags] [file...]`

Get diagnostics for one or more files. Blocks until the LSP has produced fresh results (up to a 5-second timeout by default).

Output is human-readable text by default. Errors go to stderr. Exit code 0 on success, 1 on error.

**Flags:**

| Flag | Description |
|------|-------------|
| `--no-wait` | Return stale diagnostics immediately (debugging tool, not for agent use) |
| `--timeout` | Timeout waiting for fresh diagnostics (default `5s`, `30s` for batch) |
| `--git` | Check all files changed in `git diff` against the merge base |
| `--json` | Output raw JSON instead of human-readable text |

**Examples:**

```bash
# Single file
dowse diagnostics /path/to/project/src/main.go

# Multiple files
dowse diagnostics src/main.go src/util.go

# All git-changed files
dowse diagnostics --git

# JSON output
dowse diagnostics --json src/main.go
```

### `dowse definition <file> <line> <character>`

Go to definition for the symbol at a given position. Line and character are 1-indexed. Returns the definition location(s) as JSON.

```bash
dowse definition /path/to/project/src/main.go 42 10
```

### `dowse status`

Show all active LSP sessions. Displays a table with workspace, LSP command, session status, idle time, and open file count.

```bash
dowse status
```

### `dowse mcp`

Run Dowse as an MCP (Model Context Protocol) stdio server. Designed to be launched by an AI host. Auto-starts the daemon if not running.

```bash
claude mcp add -s user -t stdio dowse -- dowse mcp
```

### `dowse dashboard [--poll <duration>]`

Open a live TUI dashboard for monitoring LSP sessions. Auto-starts the daemon if not running.

```bash
dowse dashboard
dowse dashboard --poll 5s
```

## Configuration

### Global Config

`~/.dowse/config.toml` -- applies to all workspaces.

### Workspace Config

`.dowse.toml` in the project's git root -- overrides global config for matching file extensions.

### Format

```toml
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]

[[lsp]]
extensions = [".kt", ".kts"]
command = ["kotlin-lsp"]
```

Each `[[lsp]]` entry maps file extensions to an LSP server command. Pass flags to the LSP via the `command` array.

### Full Config Reference

```toml
[daemon]
session_ttl = "30m"  # idle TTL before session is reaped (Go duration string)

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
level = "warning"  # severity filter: "error", "warning", "information", "hint" (default: "error")

[lsp.initialization_options]  # arbitrary key-value map passed to LSP initialize
verbose = true

[[lsp]]
extensions = [".kt", ".kts"]
command = ["kotlin-lsp"]
level = "hint"
```

**Fields:**

| Field | Description |
|-------|-------------|
| `extensions` | File extensions this LSP handles |
| `command` | LSP server command and arguments |
| `level` | Minimum diagnostic severity to report: `"error"`, `"warning"`, `"information"`, or `"hint"` (default: `"error"`) |
| `initialization_options` | Arbitrary key-value map passed to the LSP `initialize` request |
| `[daemon].session_ttl` | Go duration string for session idle TTL (default: `"30m"`) |

See `sample-projects/` for working config examples for Java, Kotlin, and TypeScript.

## How It Works

Dowse runs a single daemon that manages multiple LSP sessions. A session is created lazily when you first query a file -- Dowse determines the LSP from the file extension and the workspace from the git root.

Each session includes:
- An LSP server process (communicating over JSON-RPC/stdio)
- A file watcher that syncs disk changes to the LSP
- A diagnostics cache with freshness tracking

The file watcher monitors the workspace for changes to files matching the session's configured extensions, respects `.gitignore`, and sends `didOpen`/`didChange`/`didClose` notifications to the LSP as files change on disk.

For diagnostics, Dowse detects whether the LSP uses a push model (`publishDiagnostics` notifications) or pull model (`textDocument/diagnostic` requests) and handles both transparently.

## Claude Code Hook

You can wire Dowse into [Claude Code hooks](https://code.claude.com/docs/en/hooks) so that every time Claude edits or writes a file, it automatically sees LSP diagnostics and can fix errors.

Add this to your `.claude/settings.json` (project-level) or `~/.claude/settings.json` (global):

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Edit|Write",
        "hooks": [
          {
            "type": "command",
            "command": "dowse diagnostics --claude-hook"
          }
        ]
      }
    ]
  }
}
```

The `--claude-hook` flag reads the PostToolUse JSON from stdin, extracts the file path, runs diagnostics, and outputs hook-compatible JSON. If the file is clean, it exits silently. If the daemon isn't running, it fails silently to avoid blocking the model.

This requires `dowse` on your PATH. The daemon must be running (`dowse start`).

## Development

### Prerequisites

- Go 1.22+
- For integration tests: `gopls` and `kotlin-lsp` installed and on PATH

### Commands

```bash
# Build
go build -o dowse ./cmd/dowse

# Run tests
go test -race -count=1 ./...

# Run integration tests (requires gopls and kotlin-lsp installed)
go test -race -count=1 -tags=integration ./...

# Regenerate LSP protocol types from metamodel
go generate ./...

# Lint
golangci-lint run ./...

# Format
gofmt -w .

# Tidy dependencies
go mod tidy
```

### Project Structure

```
dowse/
  cmd/
    dowse/                # CLI entrypoint (start, stop, diagnostics, definition, status, mcp, dashboard)
    testwatcher/          # debug CLI for file watcher
  internal/
    daemon/               # daemon server, socket listener, request routing
    session/              # LSP session lifecycle
    lsp/
      protocol/           # generated LSP types
      generate/           # metamodel code generator
      transport/          # jrpc2 + child process wiring
    watcher/              # file watcher (fsnotify + gitignore filtering)
    cache/                # diagnostics cache + freshness tracking
    config/               # config file parsing
    mcpserver/            # MCP stdio server exposing Dowse as a tool provider
    tui/                  # terminal dashboard for monitoring LSP sessions
    integration/          # integration tests (gopls, kotlin-lsp, jdtls, typescript)
  sample-projects/        # example .dowse.toml configs for Java, Kotlin, TypeScript
```

### LSP Protocol Types

LSP types are generated from the official LSP metamodel (`metaModel.json`) via `go generate`. The generator resolves transitive type dependencies from an allow list of root types. See `internal/lsp/generate/` for the generator and `internal/lsp/protocol/` for the output.

## Daemon Files

| File | Purpose |
|------|---------|
| `~/.dowse/dowse.sock` | Unix domain socket |
| `~/.dowse/dowse.pid` | PID file |
| `~/.dowse/dowse.log` | Log file (background mode) |
| `~/.dowse/config.toml` | Global configuration |
