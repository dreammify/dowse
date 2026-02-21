# Contributing to Dowse

Thanks for your interest in contributing to Dowse! This document covers the guidelines and expectations for contributions.

## Getting Started

1. Fork and clone the repository.
2. Install Go 1.21+ and any language servers you need for testing (e.g., `gopls`).
3. Build: `go build -o dowse ./cmd/dowse`
4. Run tests: `go test -race -count=1 ./...`
5. Run integration tests: `go test -race -count=1 -tags=integration ./...`

## Code Quality Expectations

Not all parts of the codebase have the same review bar. We distinguish between two tiers:

### Relaxed areas

The following directories have a **relaxed review policy**. AI-generated code, quick experimental changes, and first-time contributions are all welcome here:

- `sample-projects/` -- example projects used for testing and demos.
- `internal/tui/` -- the terminal dashboard UI.

PRs touching only these areas will be reviewed quickly and with lighter scrutiny. They're great places to start contributing.

### Everything else

For the rest of the codebase -- the daemon, sessions, LSP transport, watcher, cache, config, MCP server, and CLI -- **code quality matters**. PRs should be:

- **Well-tested.** Add or update tests for any behavioral change. Regression tests are expected when fixing bugs.
- **Focused.** One logical change per PR. Don't mix refactors with features.
- **Concise.** Avoid over-engineering. Don't add abstractions, error handling, or configurability beyond what the change requires.
- **Consistent.** Follow the conventions in `CLAUDE.md` (variable naming, context usage, error handling patterns).

These PRs will receive thorough review and may require multiple rounds of feedback.

## Pull Request Process

1. Create a feature branch from `main`.
2. Make sure `go test -race -count=1 ./...` passes.
3. Make sure `golangci-lint run ./...` passes.
4. Open a PR with a clear description of what changed and why.
5. Keep PRs small. If a change is large, break it into a stack of smaller PRs.

## Commit Messages

Write clear, descriptive commit messages. Use imperative mood in the subject line (e.g., "Add freshness timeout for pull-model servers", not "Added freshness timeout").

## Reporting Issues

Open a GitHub issue. Include steps to reproduce, expected behavior, and actual behavior. If reporting a bug, include the output of `dowse version` and your OS.
