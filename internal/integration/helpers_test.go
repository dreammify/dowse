//go:build integration

// Package integration provides end-to-end integration tests that exercise
// the full daemon-session-LSP pipeline against real language servers.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	code := m.Run()
	teardownKotlin()
	teardownJdtls()
	teardownTypeScript()
	os.Exit(code)
}

// DiagnosticEntry mirrors daemon.EnrichedDiagnostic for JSON unmarshalling.
type DiagnosticEntry struct {
	Line     int      `json:"line"`
	EndLine  int      `json:"end_line,omitempty"`
	Severity string   `json:"severity"`
	Source   string   `json:"source,omitempty"`
	Code     string   `json:"code,omitempty"`
	Message  string   `json:"message"`
	Context  []string `json:"context"`
}

// DiagnosticsResult mirrors daemon.DiagnosticsResponse for JSON unmarshalling.
type DiagnosticsResult struct {
	File         string            `json:"file"`
	Diagnostics  []DiagnosticEntry `json:"diagnostics"`
	Stale        bool              `json:"stale"`
	ErrorCount   int               `json:"error_count"`
	WarningCount int               `json:"warning_count"`
}

var (
	builtBinary     string
	buildOnce       sync.Once
	buildErr        error
	projectRootOnce sync.Once
	projectRootVal  string
	projectRootErr  error
)

// projectRoot returns the repository root via git rev-parse --show-toplevel.
func projectRoot(t *testing.T) string {
	t.Helper()
	projectRootOnce.Do(func() {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			projectRootErr = err
			return
		}
		projectRootVal = strings.TrimSpace(string(out))
	})
	if projectRootErr != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", projectRootErr)
	}
	return projectRootVal
}

// buildDowse compiles the dowse binary once and returns its path.
func buildDowse(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "dowse-build")
		if err != nil {
			buildErr = err
			return
		}
		binaryPath := filepath.Join(tmpDir, "dowse")
		root := projectRoot(t)
		cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/dowse")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		builtBinary = binaryPath
	})
	if buildErr != nil {
		t.Fatalf("building dowse: %v", buildErr)
	}
	return builtBinary
}

// setupDowseHome creates a temporary directory for DOWSE_HOME. Uses a short
// prefix to stay under the 104-byte macOS Unix domain socket path limit.
func setupDowseHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("creating DOWSE_HOME: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// dowseInstance holds the state for a running dowse daemon.
type dowseInstance struct {
	binary    string
	dowseHome string
	env       []string
}

// newDowseInstance creates a new dowseInstance with the given binary and home.
func newDowseInstance(binary, home string) *dowseInstance {
	return &dowseInstance{
		binary:    binary,
		dowseHome: home,
		env:       append(os.Environ(), "DOWSE_HOME="+home),
	}
}

// start launches the dowse daemon and waits for it to respond to ping.
func (p *dowseInstance) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(p.binary, "start")
	cmd.Env = p.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dowse start: %v\n%s", err, out)
	}
	t.Cleanup(func() { p.stop(t) })
}

// stop sends a stop command to the daemon (best-effort).
func (p *dowseInstance) stop(t *testing.T) {
	t.Helper()
	cmd := exec.Command(p.binary, "stop")
	cmd.Env = p.env
	_ = cmd.Run()
}

// diagnostics runs dowse diagnostics on a file and parses the JSON output.
func (p *dowseInstance) diagnostics(t *testing.T, file string, extraArgs ...string) DiagnosticsResult {
	t.Helper()
	args := append([]string{"diagnostics", "--json"}, extraArgs...)
	args = append(args, file)
	cmd := exec.Command(p.binary, args...)
	cmd.Env = p.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dowse diagnostics %s: %v\n%s", file, err, out)
	}
	var result DiagnosticsResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parsing diagnostics JSON: %v\nraw output: %s", err, out)
	}
	return result
}

// diagnosticsWithRetry retries diagnostics until checkFn returns true or
// timeout expires.
func (p *dowseInstance) diagnosticsWithRetry(
	t *testing.T,
	file string,
	timeout time.Duration,
	checkFn func(DiagnosticsResult) bool,
	extraArgs ...string,
) DiagnosticsResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var result DiagnosticsResult
	for time.Now().Before(deadline) {
		args := append([]string{"diagnostics", "--json"}, extraArgs...)
		args = append(args, file)
		cmd := exec.Command(p.binary, args...)
		cmd.Env = p.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := json.Unmarshal(out, &result); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if checkFn(result) {
			return result
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("diagnostics check did not pass within %v; last result: %+v", timeout, result)
	return result
}

// DefinitionLocationEntry mirrors daemon.DefinitionLocation for JSON unmarshalling.
type DefinitionLocationEntry struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Context   string `json:"context"`
}

// DefinitionResult mirrors daemon.DefinitionResponse for JSON unmarshalling.
type DefinitionResult struct {
	Locations []DefinitionLocationEntry `json:"locations"`
}

// definition runs dowse definition on a file at a position and parses the JSON output.
func (p *dowseInstance) definition(t *testing.T, file string, line, character int) DefinitionResult {
	t.Helper()
	args := []string{"definition", file, fmt.Sprintf("%d", line), fmt.Sprintf("%d", character)}
	cmd := exec.Command(p.binary, args...)
	cmd.Env = p.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dowse definition %s:%d:%d: %v\n%s", file, line, character, err, out)
	}
	var result DefinitionResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parsing definition JSON: %v\nraw output: %s", err, out)
	}
	return result
}

// definitionWithRetry retries definition until checkFn returns true or timeout expires.
func (p *dowseInstance) definitionWithRetry(
	t *testing.T,
	file string,
	line, character int,
	timeout time.Duration,
	checkFn func(DefinitionResult) bool,
) DefinitionResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var result DefinitionResult
	for time.Now().Before(deadline) {
		args := []string{"definition", file, fmt.Sprintf("%d", line), fmt.Sprintf("%d", character)}
		cmd := exec.Command(p.binary, args...)
		cmd.Env = p.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := json.Unmarshal(out, &result); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if checkFn(result) {
			return result
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("definition check did not pass within %v; last result: %+v", timeout, result)
	return result
}

// writeFile creates all parent directories and writes content to path.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directories for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// runCmd executes a command in the given directory and fatals on error.
func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// withModifiedFile overwrites a file with new content, then restores the
// original content in t.Cleanup.
func withModifiedFile(t *testing.T, path, newContent string) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading original %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		t.Fatalf("writing modified %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0o644); err != nil {
			t.Errorf("restoring %s: %v", path, err)
		}
	})
}
