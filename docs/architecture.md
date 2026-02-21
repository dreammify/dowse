# Dowse Architecture

Dowse is an LSP bridge for AI agents. It runs a background daemon that manages LSP server connections, watches files for changes, caches diagnostics, and exposes results to AI agents via CLI, MCP, and Claude Code hooks.

## System Overview

```
                        ┌─────────────────────────────────────────────────────┐
                        │                   AI Agents                         │
                        │                                                     │
                        │  ┌──────────────┐  ┌───────────┐  ┌─────────────┐  │
                        │  │ Claude Code   │  │ Other MCP │  │  CLI/Script │  │
                        │  │ (MCP + Hook)  │  │  Clients  │  │  Workflows  │  │
                        │  └──────┬───┬────┘  └─────┬─────┘  └──────┬──────┘  │
                        └─────────┼───┼─────────────┼────────────────┼────────┘
                                  │   │             │                │
                     MCP stdio    │   │ Hook stdin  │ MCP stdio      │ CLI subprocess
                     (JSON-RPC)   │   │ (JSON)      │ (JSON-RPC)     │
                                  │   │             │                │
                        ┌─────────▼───┼─────────────▼──┐             │
                        │  dowse mcp  │                │             │
                        │  (MCP Server)│               │             │
                        │             │                │             │
                        │  Tools:     │                │             │
                        │  - get_diagnostics           │             │
                        │  - get_definition            │             │
                        └─────────┬───┼────────────────┘             │
                                  │   │                              │
                                  │   │   ┌──────────────────────────▼──────┐
                                  │   │   │  dowse diagnostics / definition │
                                  │   │   │  (CLI Commands)                 │
                                  │   │   └──────────────────┬──────────────┘
                                  │   │                      │
                        ┌─────────┼───┼──────────────────────┼──────────────┐
          Unix domain   │         │   │                      │              │
          socket HTTP   │    ┌────▼───▼──────────────────────▼────────┐     │
                        │    │           Daemon HTTP API               │     │
                        │    │  /diagnostics  /definition  /status    │     │
                        │    │  /diagnostics/batch  /shutdown  /ping  │     │
                        │    │  /session-status  /dashboard           │     │
                        │    │  /session/kill                         │     │
                        │    └────────────────────┬───────────────────┘     │
                        │                         │                         │
                        │    ┌────────────────────▼───────────────────┐     │
                        │    │         Session Manager                │     │
                        │    │  (lazy creation, singleflight dedup)  │     │
                        │    │  sessions keyed by workspace+lsp_cmd  │     │
                        │    └───┬──────────────┬──────────────┬─────┘     │
                        │        │              │              │            │
                        │   ┌────▼────┐   ┌────▼────┐   ┌────▼────┐       │
                        │   │Session 1│   │Session 2│   │Session N│       │
                        │   │ (.go)   │   │ (.kt)   │   │ (.ts)   │       │
                        │   └────┬────┘   └────┬────┘   └────┬────┘       │
                        │        │              │              │            │
                        │  ┌─────▼──────────────▼──────────────▼─────┐     │
                        │  │    Per-Session Components                │     │
                        │  │                                         │     │
                        │  │  ┌─────────┐  ┌─────────┐  ┌────────┐  │     │
                        │  │  │ Watcher │  │  Cache  │  │Progress│  │     │
                        │  │  │(fsnotify)│  │(diags) │  │Tracker │  │     │
                        │  │  └────┬────┘  └────┬────┘  └────────┘  │     │
                        │  └───────┼────────────┼────────────────────┘     │
                        │          │            │                           │
                        │  Daemon  │            │                           │
                        └──────────┼────────────┼───────────────────────────┘
                                   │            │
                        ┌──────────▼────────────▼───────────────────────────┐
                        │          LSP Transport (JSON-RPC over stdio)      │
                        │          jrpc2 client + Content-Length framing     │
                        └──────────┬────────────┬──────────────┬────────────┘
                                   │            │              │
                        ┌──────────▼──┐  ┌──────▼───┐  ┌──────▼──────────┐
                        │   gopls     │  │kotlin-lsp│  │typescript-      │
                        │  (push)     │  │  (pull)  │  │language-server  │
                        └─────────────┘  └──────────┘  └─────────────────┘
```

## Core Components

### Daemon (`internal/daemon/`)

The central process. Listens on a Unix domain socket (`~/.dowse/dowse.sock`) and exposes an HTTP API. Manages the lifecycle of all LSP sessions.

Key responsibilities:
- Session creation, lookup, and deactivation (idle TTL reaper)
- Config resolution (walking up to find `.dowse.toml`)
- Routing file watcher events to the correct session
- Wiring diagnostics from LSP servers into the cache
- Formatting diagnostics/definition responses with code context

Sessions are keyed by `(workspace_root, lsp_command)` and created lazily on the first request for a given workspace and file extension. A `singleflight.Group` prevents concurrent requests from racing to create the same session.

### Session (`internal/session/`)

One session per workspace+LSP combination. Wraps an LSP process and handles the protocol lifecycle:

1. Spawn LSP child process via transport
2. Send `initialize` / `initialized` handshake
3. Detect push vs pull diagnostic model from `ServerCapabilities`
4. Dispatch notifications (`publishDiagnostics`, `$/progress`) and callbacks (`workDoneProgress/create`)
5. Expose `OpenFile`, `ChangeFile`, `CloseFile`, `PullDiagnostics`, `Definition`

### Transport (`internal/lsp/transport/`)

JSON-RPC 2.0 over stdio pipes using `creachadair/jrpc2`. Handles Content-Length framing with a lenient parser that tolerates non-standard headers from servers like Kotlin LSP.

### Cache (`internal/cache/`)

Per-file diagnostics cache with freshness tracking. Supports blocking waits so that CLI requests can block until diagnostics are up-to-date after a file change.

Freshness is determined by:
- **Version-based**: Diagnostics version >= the version sent with the file change (used by gopls)
- **Timestamp-based**: Fallback when versions aren't available
- **Sequence counters**: Monotonic counters prevent an Update received before a RecordChange from being treated as fresh

### Watcher (`internal/watcher/`)

Per-session file watcher using `fsnotify`. Watches directories recursively, filters by configured file extensions, respects `.gitignore` (via `git check-ignore`), and debounces events (50ms per file). New directories are auto-added to the watch set.

### Config (`internal/config/`)

Parses TOML configuration from two layers:
- **Global**: `~/.dowse/config.toml`
- **Workspace**: `.dowse.toml` (found by walking up from the queried file)

Workspace config overrides global on a per-extension basis. Maps file extensions to LSP commands and controls severity filtering, session TTL, and MCP tool exposure.

```toml
[daemon]
session_ttl = "30m"

[mcp]
tools = ["get_diagnostics"]  # omit to expose all tools

[[lsp]]
extensions = [".go"]
command = ["gopls"]
level = "error"
```

### MCP Server (`internal/mcpserver/`)

MCP (Model Context Protocol) stdio server using newline-delimited JSON-RPC. Exposes tools to AI agent hosts:

| Tool | Description |
|------|-------------|
| `get_diagnostics` | Diagnostics for one or more files (accepts a `files` array) |
| `get_definition` | Go-to-definition (file, line, character) |

Tool metadata (names, descriptions, parameter schemas) is defined in embedded TOML files under `internal/mcpserver/tools/`, loaded at init via `//go:embed`. This separates tool authoring from implementation code. The `[mcp]` config section allows restricting which tools are exposed (see Config above).

Communicates with the daemon via Unix socket HTTP, same as the CLI.

### TUI Dashboard (`internal/tui/`)

Live terminal dashboard built with `tview`/`tcell`. Polls the daemon's `/dashboard` endpoint and displays session status, resource usage (CPU%, RSS via `gopsutil`), diagnostics counts, and idle times. Supports killing sessions and stopping/starting the daemon.

### Protocol Types (`internal/lsp/protocol/`)

Generated from the official LSP `metaModel.json`. Uses an allowlist of ~25 root types with transitive dependency resolution. Regenerate with `go generate ./...`.

## AI Agent Integration

Dowse provides three integration paths for AI agents:

### 1. MCP Server (primary)

The recommended integration. The AI host (e.g., Claude Code) launches `dowse mcp` as an MCP stdio server. The MCP server auto-starts the daemon if needed and forwards tool calls to the daemon over the Unix socket.

```
AI Host  <──MCP stdio──>  dowse mcp  <──HTTP/Unix socket──>  Daemon
```

### 2. Claude Code Hook (reactive)

`dowse diagnostics --claude-hook` acts as a PostToolUse hook. After Claude edits a file, the hook reads the tool output from stdin, extracts the file path, queries the daemon, and returns diagnostics as `additionalContext` in the hook response. Clean files produce no output to save context tokens.

```
Claude Code  ──PostToolUse JSON──>  dowse diagnostics --claude-hook  ──>  Daemon
         <──hook JSON with diagnostics──
```

### 3. CLI (manual/scripted)

Standard CLI commands (`dowse diagnostics`, `dowse definition`) can be invoked as subprocesses from any agent framework.

## Data Flow

### Push Model (e.g., gopls)

```
File change on disk
    │
    ▼
Watcher (fsnotify, debounced)
    │
    ▼
Daemon reads file, sends didOpen/didChange to Session
    │
    ▼
Session forwards to LSP via JSON-RPC
    │
    ▼
LSP server analyzes and publishes diagnostics
    │
    ▼
Session receives publishDiagnostics notification
    │
    ▼
Daemon's OnDiagnostics callback stores in Cache
    │
    ▼
Cache broadcasts to waiters (WaitForFresh unblocks)
    │
    ▼
Response returned to CLI/MCP client
```

### Pull Model (e.g., Kotlin LSP)

```
CLI/MCP request arrives
    │
    ▼
Daemon detects pull model for session
    │
    ▼
Session sends textDocument/diagnostic request
    │
    ▼
LSP server responds with diagnostics directly
    │
    ▼
Daemon formats and returns response
```

## Session Lifecycle

1. **First request** for a workspace+extension triggers `getOrCreateSession`
2. **Config resolution**: Walk up from file to find `.dowse.toml`, resolve extension to LSP command
3. **Spawn**: `session.New()` starts LSP child process with stdio pipes
4. **Initialize**: LSP handshake, detect push/pull from `ServerCapabilities`
5. **Wire up**: Register `OnDiagnostics` callback to feed the cache
6. **Watch**: Start file watcher for the workspace with matching extensions
7. **Serve**: Handle diagnostics/definition requests
8. **Reap**: Session reaper deactivates sessions idle longer than TTL (default 30 min)
9. **Revive**: Next request for a deactivated session creates a new one

## Process Model

Go cannot `fork()`, so `dowse start` re-executes the binary as `dowse daemon` (a hidden subcommand) with `SysProcAttr.Setsid = true` to detach from the terminal. The daemon's stdout/stderr redirect to `~/.dowse/dowse.log`.

State files live under `$DOWSE_HOME` (default `~/.dowse/`):

| File | Purpose |
|------|---------|
| `dowse.sock` | Unix domain socket |
| `dowse.pid` | Daemon PID |
| `dowse.log` | Daemon log output |
| `config.toml` | Global configuration |
