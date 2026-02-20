package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/spf13/cobra"
)

func newDiagnosticsCmd() *cobra.Command {
	var (
		noWait   bool
		timeout  time.Duration
		gitFlag  bool
		jsonFlag bool
		claudeHookFlag bool
	)

	cmd := &cobra.Command{
		Use:   "diagnostics [file...]",
		Short: "Get LSP diagnostics for files",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if claudeHookFlag {
				return runHookMode()
			}

			var files []string

			if gitFlag {
				gitFiles, err := gitDiffFiles(cmd.Context())
				if err != nil {
					return fmt.Errorf("git diff: %w", err)
				}
				files = append(files, gitFiles...)
			}

			for _, arg := range args {
				absPath, err := filepath.Abs(arg)
				if err != nil {
					return fmt.Errorf("resolving path %s: %w", arg, err)
				}
				files = append(files, absPath)
			}

			if len(files) == 0 {
				return fmt.Errorf("provide at least one file or use --git")
			}

			// When the user hasn't explicitly set --timeout, send 0 to let
			// the daemon choose (it uses a longer timeout for the first
			// request in a session to accommodate LSP cold starts).
			explicitTimeout := cmd.Flags().Changed("timeout")

			// Use 30s default timeout for batch requests (cold-start friendly).
			if len(files) > 1 && !explicitTimeout {
				timeout = 30 * time.Second
				explicitTimeout = true
			}

			socketPath := daemon.DefaultSocketPath()

			// When no explicit timeout, the daemon may wait up to its
			// firstRequestTimeout (1 min) on cold start. Set the HTTP
			// client timeout high enough to not cut it short.
			httpTimeout := timeout + 2*time.Second
			if !explicitTimeout {
				httpTimeout = 90 * time.Second
			}

			client := daemon.NewUnixClient(socketPath, httpTimeout)

			if len(files) > 1 {
				return postBatchDiagnostics(client, files, noWait, timeout, jsonFlag)
			}

			var reqTimeout float64
			if explicitTimeout {
				reqTimeout = timeout.Seconds()
			}

			filePath := files[0]
			reqBody, err := json.Marshal(daemon.DiagnosticsRequest{
				File:    filePath,
				NoWait:  noWait,
				Timeout: reqTimeout,
			})
			if err != nil {
				return fmt.Errorf("marshaling request: %w", err)
			}

			// Run the diagnostics request in a goroutine so we can poll status.
			type result struct {
				body       []byte
				statusCode int
				err        error
			}
			resultCh := make(chan result, 1)
			go func() {
				resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(reqBody))
				if err != nil {
					resultCh <- result{err: fmt.Errorf("connecting to daemon: %w (is the daemon running?)", err)}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					resultCh <- result{err: fmt.Errorf("reading response: %w", err)}
					return
				}
				resultCh <- result{body: body, statusCode: resp.StatusCode}
			}()

			// If response doesn't arrive within 2s and we're not in no-wait mode,
			// start polling session status every 5s.
			if !noWait {
				statusClient := daemon.NewUnixClient(socketPath, 2*time.Second)
				statusDelay := time.NewTimer(2 * time.Second)
				defer statusDelay.Stop()

				select {
				case res := <-resultCh:
					return handleDiagnosticsResult(res.body, res.statusCode, res.err, jsonFlag)
				case <-statusDelay.C:
					// Start status polling.
				}

				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()

				// Print first status immediately after the 2s delay.
				printSessionStatus(statusClient, filePath)

				for {
					select {
					case res := <-resultCh:
						return handleDiagnosticsResult(res.body, res.statusCode, res.err, jsonFlag)
					case <-ticker.C:
						printSessionStatus(statusClient, filePath)
					}
				}
			}

			res := <-resultCh
			return handleDiagnosticsResult(res.body, res.statusCode, res.err, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return stale diagnostics immediately")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "timeout waiting for fresh diagnostics")
	cmd.Flags().BoolVar(&gitFlag, "git", false, "check all files changed in git diff")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "output raw JSON instead of human-readable text")
	cmd.Flags().BoolVar(&claudeHookFlag, "claude-hook", false, "run as a Claude Code PostToolUse hook (reads stdin, outputs hook JSON)")
	return cmd
}

// runHookMode reads a PostToolUse hook JSON payload from stdin, extracts the
// file path, runs diagnostics, and outputs Claude Code hook-compatible JSON.
// If the file is clean (no diagnostics), it exits silently with code 0.
func runHookMode() error {
	var hookInput struct {
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&hookInput); err != nil {
		return fmt.Errorf("reading hook input: %w", err)
	}

	filePath := hookInput.ToolInput.FilePath
	if filePath == "" {
		// No file path in input (e.g. tool doesn't have file_path), nothing to do.
		return nil
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}

	socketPath := daemon.DefaultSocketPath()
	client := daemon.NewUnixClient(socketPath, 90*time.Second)

	reqBody, err := json.Marshal(daemon.DiagnosticsRequest{File: absPath})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		// Daemon not running or unreachable — fail silently so we don't
		// block the model's workflow.
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var diagResp daemon.DiagnosticsResponse
	if err := json.Unmarshal(body, &diagResp); err != nil {
		return nil
	}

	if len(diagResp.Diagnostics) == 0 {
		return nil
	}

	// Format diagnostics using the existing pretty-printer.
	text := formatDiagnosticsText(diagResp)

	hookOutput := struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}{}
	hookOutput.HookSpecificOutput.HookEventName = "PostToolUse"
	hookOutput.HookSpecificOutput.AdditionalContext = text

	return json.NewEncoder(os.Stdout).Encode(hookOutput)
}

func handleDiagnosticsResult(body []byte, statusCode int, err error, jsonOutput bool) error {
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("daemon error: %s", string(body))
	}
	if jsonOutput {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			return fmt.Errorf("formatting JSON: %w", err)
		}
		fmt.Fprintln(os.Stdout, buf.String())
		return nil
	}
	var resp daemon.DiagnosticsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	fmt.Fprint(os.Stdout, formatDiagnosticsText(resp))
	return nil
}

func postBatchDiagnostics(client *http.Client, files []string, noWait bool, timeout time.Duration, jsonOutput bool) error {
	reqBody, err := json.Marshal(daemon.BatchDiagnosticsRequest{
		Files:   files,
		NoWait:  noWait,
		Timeout: timeout.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := client.Post("http://dowse/diagnostics/batch", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w (is the daemon running?)", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon error: %s", string(body))
	}

	if jsonOutput {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			return fmt.Errorf("formatting JSON: %w", err)
		}
		fmt.Fprintln(os.Stdout, buf.String())
		return nil
	}
	var batchResp daemon.BatchDiagnosticsResponse
	if err := json.Unmarshal(body, &batchResp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	fmt.Fprint(os.Stdout, formatBatchDiagnosticsText(batchResp))
	return nil
}

// gitDiffFiles returns absolute paths of files changed on the current branch.
// It compares against the merge base with the default branch (main/master) so
// that committed branch changes are included, not just uncommitted edits.
// Untracked files are also included since they represent new work.
func gitDiffFiles(ctx context.Context) ([]string, error) {
	rootCmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	rootOut, err := rootCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("finding git root: %w", err)
	}
	gitRoot := strings.TrimSpace(string(rootOut))

	base, err := diffBase(ctx, gitRoot)
	if err != nil {
		return nil, err
	}

	diffCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", base)
	diffOut, err := diffCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	// Also pick up untracked files (new files not yet added to git).
	untrackedCmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	untrackedOut, err := untrackedCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	seen := make(map[string]struct{})
	var files []string
	for _, raw := range [][]byte{diffOut, untrackedOut} {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			absPath := filepath.Join(gitRoot, line)
			if _, ok := seen[absPath]; ok {
				continue
			}
			seen[absPath] = struct{}{}
			files = append(files, absPath)
		}
	}
	return files, nil
}

// diffBase returns the best ref to diff against: the merge-base with the
// default branch if available, otherwise HEAD.
func diffBase(ctx context.Context, gitRoot string) (string, error) {
	// Try common default branch names.
	for _, branch := range []string{"main", "master"} {
		mergeBaseCmd := exec.CommandContext(ctx, "git", "-C", gitRoot, "merge-base", "HEAD", branch)
		out, err := mergeBaseCmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	// Fallback: compare against HEAD (uncommitted changes only).
	return "HEAD", nil
}

func printSessionStatus(client *http.Client, filePath string) {
	resp, err := client.Get("http://dowse/session-status?file=" + url.QueryEscape(filePath))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var status daemon.SessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return
	}

	if status.Status == "ready" {
		return
	}

	statusStr := status.Status
	if status.Percent != nil {
		statusStr = fmt.Sprintf("%s (%d%%)", status.Status, *status.Percent)
	}
	fmt.Fprintf(os.Stderr, "dowse: waiting for diagnostics... (%s)\n", statusStr)
}
