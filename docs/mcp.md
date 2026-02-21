# MCP Server

Dowse includes an MCP (Model Context Protocol) server that exposes diagnostics and go-to-definition as tools over stdio. This lets AI hosts like Claude Desktop or Claude Code use Dowse directly without shell commands.

## Setup

Register Dowse as an MCP server with your AI host. For Claude Code:

```bash
claude mcp add -s user -t stdio dowse -- dowse mcp
```

The daemon is auto-started if not already running.

## Tools

### `get_diagnostics`

Get LSP diagnostics (errors, warnings) for one or more files.

**Parameters:**

| Name | Type | Required | Description |
|------|------|----------|-------------|
| `files` | `string[]` | yes | Absolute paths to the files to check |

**Example call:**

```json
{
  "name": "get_diagnostics",
  "arguments": {
    "files": ["/path/to/project/src/main.go", "/path/to/project/src/util.go"]
  }
}
```

### `get_definition`

Go to definition for a symbol at a given position in a file. Returns the file path, line number, and character offset of the definition along with the source line.

**Parameters:**

| Name | Type | Required | Description |
|------|------|----------|-------------|
| `file` | `string` | yes | Absolute path to the file containing the symbol |
| `line` | `integer` | yes | Line number of the symbol (1-indexed) |
| `character` | `integer` | yes | Character offset within the line (1-indexed) |

**Example call:**

```json
{
  "name": "get_definition",
  "arguments": {
    "file": "/path/to/project/src/main.go",
    "line": 42,
    "character": 10
  }
}
```

## Configuring Exposed Tools

By default all tools are exposed. To restrict which tools are available, add an `[mcp]` section to your global config (`~/.dowse/config.toml`):

```toml
[mcp]
tools = ["get_diagnostics"]
```

Only the listed tools will appear in the `tools/list` response. Calls to non-listed tools return an error. Omitting the `[mcp]` section or leaving `tools` empty exposes all tools.

This is a global-level setting — it is not configurable per workspace.

## Tool Definitions

Tool metadata (names, descriptions, parameter schemas) is defined in TOML files under `internal/mcpserver/tools/`. These files are embedded into the binary at compile time. To modify a tool's description or parameter documentation, edit the corresponding `.toml` file and rebuild.

```
internal/mcpserver/tools/
  get_diagnostics.toml
  get_definition.toml
```

Each file follows this structure:

```toml
name = "tool_name"
description = "What the tool does."

[parameters.param_name]
type = "string"
description = "What this parameter is."
required = true
```

Array parameters use a nested `items` table:

```toml
[parameters.files]
type = "array"
description = "List of file paths."
required = true

[parameters.files.items]
type = "string"
```

## Architecture

```
AI Host (Claude, etc.)
  --stdio (newline-delimited JSON-RPC)-->
    MCP Server (dowse mcp)
      --HTTP over Unix socket-->
        Dowse Daemon (~/.dowse/dowse.sock)
          --LSP JSON-RPC-->
            Language Server (gopls, etc.)
```

The MCP server is a thin bridge. It translates MCP tool calls into HTTP requests to the daemon, which manages the actual LSP sessions. The daemon is auto-started by `dowse mcp` if not already running.

The server implements MCP protocol version `2025-03-26` using `creachadair/jrpc2` for JSON-RPC transport.
