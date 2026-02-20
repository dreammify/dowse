# Claude Code Hook

Dowse integrates with [Claude Code hooks](https://code.claude.com/docs/en/hooks) to automatically show LSP diagnostics after every file edit. When Claude writes or edits a file and introduces an error, it sees the diagnostics immediately and can fix them in the same turn.

## Setup

1. Start the daemon:

   ```bash
   dowse start
   ```

2. Add the hook to your `.claude/settings.json` (project-level) or `~/.claude/settings.json` (global):

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

This requires `dowse` on your PATH.

## How it works

The `--claude-hook` flag changes `dowse diagnostics` to operate as a Claude Code PostToolUse hook handler:

1. Reads the PostToolUse JSON payload from stdin
2. Extracts `tool_input.file_path` from the payload
3. Sends a diagnostics request to the daemon for that file
4. If diagnostics are found, outputs hook-compatible JSON with the diagnostics as `additionalContext`
5. If the file is clean (no errors or warnings), exits silently with code 0

The diagnostics are formatted the same way as the regular `dowse diagnostics` output:

```
  ✘ [Line 42] undeclared name 'foo'
    41│ func main() {
    42│     fmt.Println(foo)
    43│ }

  ⚠ [Line 10] unused variable 'bar'
    9│ func init() {
   10│     bar := 1
   11│ }
```

## Behavior

- **Clean files produce no output.** When there are no diagnostics, the hook exits 0 with no stdout. This avoids wasting context tokens on files that have no issues.
- **Errors are silent.** If the daemon isn't running, the socket is unreachable, or the file's extension has no configured LSP, the hook exits 0 silently. This prevents hook failures from blocking Claude's workflow.
- **No dependencies.** Unlike a shell-pipeline approach, `--claude-hook` handles stdin parsing and JSON output formatting internally. No `jq` or other tools required.

## Matching

The `"matcher": "Edit|Write"` pattern fires the hook after Claude's `Edit` and `Write` tool calls. Both tools include a `file_path` field in their input, which is what the hook reads.

Other tools that also have `file_path` (like `Read`) would work but aren't useful since reading a file doesn't change it.
