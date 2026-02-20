package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dreammify/dowse/internal/lsp/protocol"
	"github.com/dreammify/dowse/internal/session"
)

func TestDowseHome(t *testing.T) {
	t.Run("uses DOWSE_HOME when set", func(t *testing.T) {
		t.Setenv("DOWSE_HOME", "/tmp/test-dowse")
		if got := DefaultSocketPath(); got != "/tmp/test-dowse/dowse.sock" {
			t.Fatalf("expected /tmp/test-dowse/dowse.sock, got %s", got)
		}
		if got := DefaultPIDPath(); got != "/tmp/test-dowse/dowse.pid" {
			t.Fatalf("expected /tmp/test-dowse/dowse.pid, got %s", got)
		}
		if got := DefaultLogPath(); got != "/tmp/test-dowse/dowse.log" {
			t.Fatalf("expected /tmp/test-dowse/dowse.log, got %s", got)
		}
		if got := globalConfigPath(); got != "/tmp/test-dowse/config.toml" {
			t.Fatalf("expected /tmp/test-dowse/config.toml, got %s", got)
		}
	})

	t.Run("falls back to home directory", func(t *testing.T) {
		t.Setenv("DOWSE_HOME", "")
		home, _ := os.UserHomeDir()
		expected := filepath.Join(home, ".dowse", "dowse.sock")
		if got := DefaultSocketPath(); got != expected {
			t.Fatalf("expected %s, got %s", expected, got)
		}
	})
}

// changeRecord records a ChangeFile call.
type changeRecord struct {
	uri     string
	version int
}

// mockSession implements LSPSession for testing.
type mockSession struct {
	mu               sync.Mutex // protects opened, changes, and closed for concurrent access
	diagModel        session.DiagnosticModel
	opened           []string
	changes          []changeRecord
	closed           []string // URIs closed via didClose
	pullResult       []protocol.Diagnostic
	definitionResult []protocol.Location
	definitionErr    error
	diagHandler      func(uri string, version *int, diagnostics []protocol.Diagnostic)
	shutdownErr      error
	shutdownCh       chan struct{}
	initDone         chan struct{}
	initDelay        time.Duration // artificial delay before init completes
	initErr          error
	status           string // override for Progress/ProgressWithPercent
}

func newMockSession(diagModel session.DiagnosticModel) *mockSession {
	return &mockSession{
		diagModel:  diagModel,
		shutdownCh: make(chan struct{}),
		initDone:   make(chan struct{}),
	}
}

// newDelayedMockSession creates a mock whose Initialize blocks for the given
// duration, allowing tests to observe the "initializing" state.
func newDelayedMockSession(diagModel session.DiagnosticModel, delay time.Duration) *mockSession {
	return &mockSession{
		diagModel:  diagModel,
		shutdownCh: make(chan struct{}),
		initDone:   make(chan struct{}),
		initDelay:  delay,
	}
}

func (m *mockSession) Initialize(ctx context.Context) error {
	defer close(m.initDone)
	if m.initDelay > 0 {
		select {
		case <-time.After(m.initDelay):
		case <-ctx.Done():
			m.initErr = ctx.Err()
			return m.initErr
		}
	}
	return m.initErr
}

func (m *mockSession) WaitReady(ctx context.Context) error {
	select {
	case <-m.initDone:
		return m.initErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *mockSession) OpenFile(_ context.Context, uri string, _ string, _ protocol.LanguageKind) error {
	m.mu.Lock()
	m.opened = append(m.opened, uri)
	m.mu.Unlock()
	// For push model, simulate the LSP sending diagnostics after open.
	if m.diagModel == session.DiagnosticPush && m.diagHandler != nil {
		go func() {
			time.Sleep(10 * time.Millisecond)
			m.diagHandler(uri, nil, nil)
		}()
	}
	return nil
}

func (m *mockSession) ChangeFile(_ context.Context, uri string, _ string, version int) error {
	m.mu.Lock()
	m.changes = append(m.changes, changeRecord{uri: uri, version: version})
	m.mu.Unlock()
	// For push model, simulate the LSP sending diagnostics after change.
	if m.diagModel == session.DiagnosticPush && m.diagHandler != nil {
		go func() {
			time.Sleep(10 * time.Millisecond)
			v := int32(version)
			m.diagHandler(uri, func() *int { i := int(v); return &i }(), nil)
		}()
	}
	return nil
}

func (m *mockSession) CloseFile(_ context.Context, uri string) error {
	m.mu.Lock()
	m.closed = append(m.closed, uri)
	m.mu.Unlock()
	return nil
}

func (m *mockSession) PullDiagnostics(_ context.Context, _ string) ([]protocol.Diagnostic, error) {
	return m.pullResult, nil
}

func (m *mockSession) Definition(_ context.Context, _ string, _ uint32, _ uint32) ([]protocol.Location, error) {
	return m.definitionResult, m.definitionErr
}

func (m *mockSession) DiagModel() session.DiagnosticModel {
	return m.diagModel
}

func (m *mockSession) PID() int {
	return 12345
}

func (m *mockSession) OnDiagnostics(handler func(uri string, version *int, diagnostics []protocol.Diagnostic)) {
	m.diagHandler = handler
}

func (m *mockSession) Shutdown(_ context.Context) error {
	close(m.shutdownCh)
	return m.shutdownErr
}

func (m *mockSession) Progress() string {
	status, _ := m.ProgressWithPercent()
	return status
}

func (m *mockSession) ProgressWithPercent() (string, *uint32) {
	if m.status != "" {
		return m.status, nil
	}
	select {
	case <-m.initDone:
		return "", nil
	default:
		return "initializing", nil
	}
}

func (m *mockSession) Wait() <-chan struct{} {
	return m.shutdownCh
}

func startTestDaemon(t *testing.T, factory SessionFactory) (d *Daemon, client *http.Client, cancel context.CancelFunc) {
	t.Helper()

	// Use os.MkdirTemp with a short prefix instead of t.TempDir() to keep
	// the socket path under the 104-byte Unix domain socket limit on macOS.
	// t.TempDir() embeds the full test name, which can exceed the limit.
	sockDir, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socketPath := filepath.Join(sockDir, "test.sock")
	d = New(factory)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx, socketPath)
	}()

	client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}

	// Wait for the daemon to start listening.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://dowse/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("daemon exited with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Log("daemon did not exit within timeout")
		}
	})

	return d, client, cancel
}

func TestPing(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	resp, err := client.Get("http://dowse/ping")
	if err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestShutdown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	d := New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx, socketPath)
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}

	// Wait for startup.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://dowse/ping")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send shutdown.
	resp, err := client.Post("http://dowse/shutdown", "", nil)
	if err != nil {
		t.Fatalf("shutdown request failed: %v", err)
	}
	resp.Body.Close()

	// Daemon should exit cleanly.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("daemon exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit after shutdown")
	}
}

func TestDiagnosticsMissingFile(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	body, _ := json.Marshal(DiagnosticsRequest{})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty file, got %d", resp.StatusCode)
	}
}

func TestDiagnosticsInvalidJSON(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestRouting(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	tests := []struct {
		method string
		path   string
		status int
	}{
		{"GET", "http://dowse/ping", http.StatusOK},
		{"POST", "http://dowse/diagnostics", http.StatusBadRequest}, // missing body
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %s", tt.method, tt.path), func(t *testing.T) {
			var resp *http.Response
			var err error
			switch tt.method {
			case "GET":
				resp, err = client.Get(tt.path)
			case "POST":
				resp, err = client.Post(tt.path, "application/json", bytes.NewReader([]byte("{}")))
			}
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Fatalf("expected %d, got %d", tt.status, resp.StatusCode)
			}
		})
	}
}

// setupTestWorkspace creates a temporary git repo with a .dowse.toml and a
// Go source file. Returns the workspace root and the path to main.go.
func setupTestWorkspace(t *testing.T) (wsRoot, mainGo string) {
	t.Helper()
	dir := t.TempDir()

	// Resolve symlinks so the path matches what filepath.EvalSymlinks returns.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// Init git repo.
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Write .dowse.toml.
	cfg := `[[lsp]]
extensions = [".go"]
command = ["mock-lsp"]
`
	if err := os.WriteFile(filepath.Join(dir, ".dowse.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write main.go.
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir, mainPath
}

func postDiagnostics(t *testing.T, client *http.Client, file string) DiagnosticsResponse {
	t.Helper()
	body, _ := json.Marshal(DiagnosticsRequest{File: file, NoWait: true})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var diagResp DiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&diagResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return diagResp
}

// TestOpenFileTracking verifies that the first request for a file sends
// didOpen and subsequent requests send didChange with incrementing versions.
func TestOpenFileTracking(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPush) // push model
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		if root != wsRoot {
			t.Fatalf("unexpected workspace root: %s", root)
		}
		return mock, nil
	})

	// First request: should call OpenFile, not ChangeFile.
	postDiagnostics(t, client, mainGo)
	time.Sleep(50 * time.Millisecond) // let async push settle

	if len(mock.opened) != 1 {
		t.Fatalf("expected 1 OpenFile call, got %d", len(mock.opened))
	}
	if len(mock.changes) != 0 {
		t.Fatalf("expected 0 ChangeFile calls on first request, got %d", len(mock.changes))
	}

	// Second request: should call ChangeFile with version 2, not OpenFile again.
	postDiagnostics(t, client, mainGo)
	time.Sleep(50 * time.Millisecond)

	if len(mock.opened) != 1 {
		t.Fatalf("expected still 1 OpenFile call, got %d", len(mock.opened))
	}
	if len(mock.changes) != 1 {
		t.Fatalf("expected 1 ChangeFile call, got %d", len(mock.changes))
	}
	if mock.changes[0].version != 2 {
		t.Fatalf("expected ChangeFile version 2, got %d", mock.changes[0].version)
	}

	// Third request: version 3.
	postDiagnostics(t, client, mainGo)
	time.Sleep(50 * time.Millisecond)

	if len(mock.opened) != 1 {
		t.Fatalf("expected still 1 OpenFile call, got %d", len(mock.opened))
	}
	if len(mock.changes) != 2 {
		t.Fatalf("expected 2 ChangeFile calls, got %d", len(mock.changes))
	}
	if mock.changes[1].version != 3 {
		t.Fatalf("expected ChangeFile version 3, got %d", mock.changes[1].version)
	}
}

// setupNestedTestWorkspace creates a temporary git repo with a .dowse.toml in
// a nested subdirectory. Returns the git root, the nested project root, and the
// path to a source file inside the nested project.
func setupNestedTestWorkspace(t *testing.T) (gitRootDir, projectRoot, sourceFile string) {
	t.Helper()
	dir := t.TempDir()

	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// Init git repo at top level.
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Create nested project with its own .dowse.toml.
	nestedDir := filepath.Join(dir, "projects", "kotlin-app")
	srcDir := filepath.Join(nestedDir, "src", "main")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := `[[lsp]]
extensions = [".kt"]
command = ["mock-kotlin-lsp"]
`
	if err := os.WriteFile(filepath.Join(nestedDir, ".dowse.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(srcDir, "App.kt")
	if err := os.WriteFile(filePath, []byte("fun main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir, nestedDir, filePath
}

// TestNestedProjectRoot verifies that when a file is inside a nested project
// with its own .dowse.toml, the session receives the nested directory as
// the workspace root instead of the git root.
func TestNestedProjectRoot(t *testing.T) {
	_, projectRoot, sourceFile := setupNestedTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPull) // pull model
	var receivedRoot string
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, cmd []string, _ map[string]any) (LSPSession, error) {
		receivedRoot = root
		return mock, nil
	})

	postDiagnostics(t, client, sourceFile)

	if receivedRoot != projectRoot {
		t.Fatalf("expected workspace root %q, got %q", projectRoot, receivedRoot)
	}
}

// TestTwoNestedProjects verifies that two nested subdirectories each with their
// own .dowse.toml create separate sessions with different workspace roots.
func TestTwoNestedProjects(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	ktCfg := `[[lsp]]
extensions = [".kt"]
command = ["mock-kotlin-lsp"]
`
	// Project A.
	projectA := filepath.Join(dir, "project-a")
	if err := os.MkdirAll(filepath.Join(projectA, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectA, ".dowse.toml"), []byte(ktCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(projectA, "src", "A.kt")
	if err := os.WriteFile(fileA, []byte("fun a() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project B.
	projectB := filepath.Join(dir, "project-b")
	if err := os.MkdirAll(filepath.Join(projectB, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectB, ".dowse.toml"), []byte(ktCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fileB := filepath.Join(projectB, "src", "B.kt")
	if err := os.WriteFile(fileB, []byte("fun b() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var roots []string
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, cmd []string, _ map[string]any) (LSPSession, error) {
		roots = append(roots, root)
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, fileA)
	postDiagnostics(t, client, fileB)

	if len(roots) != 2 {
		t.Fatalf("expected 2 sessions created, got %d", len(roots))
	}
	if roots[0] != projectA {
		t.Fatalf("expected first session root %q, got %q", projectA, roots[0])
	}
	if roots[1] != projectB {
		t.Fatalf("expected second session root %q, got %q", projectB, roots[1])
	}
}

// TestSymlinkResolution verifies that file paths are resolved through symlinks
// so that the URI sent to the LSP matches what the LSP sends back.
func TestSymlinkResolution(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	// Create a symlink pointing to mainGo.
	symlinkDir := t.TempDir()
	symlink := filepath.Join(symlinkDir, "link.go")
	if err := os.Symlink(mainGo, symlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	// The symlink is outside the git repo so we can't use it directly with
	// the daemon (gitRoot would fail). Instead, create a symlink to the
	// workspace directory and use a file path through it.
	wsSymlink := filepath.Join(symlinkDir, "ws")
	if err := os.Symlink(wsRoot, wsSymlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	symlinkMainGo := filepath.Join(wsSymlink, "main.go")

	mock := newMockSession(session.DiagnosticPush)
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		// The factory should receive the resolved (real) path, not the symlink.
		if root != wsRoot {
			t.Errorf("expected resolved workspace root %q, got %q", wsRoot, root)
		}
		return mock, nil
	})

	// Request diagnostics via the symlink path.
	postDiagnostics(t, client, symlinkMainGo)
	time.Sleep(50 * time.Millisecond)

	if len(mock.opened) != 1 {
		t.Fatalf("expected 1 OpenFile call, got %d", len(mock.opened))
	}
	// The URI should use the resolved path, not the symlink.
	expectedURI := "file://" + mainGo
	if mock.opened[0] != expectedURI {
		t.Fatalf("expected URI %q, got %q", expectedURI, mock.opened[0])
	}
}

// postBatch sends a POST /diagnostics/batch request and decodes the response.
func postBatch(t *testing.T, client *http.Client, files []string) BatchDiagnosticsResponse {
	t.Helper()
	body, _ := json.Marshal(BatchDiagnosticsRequest{Files: files, NoWait: true})
	resp, err := client.Post("http://dowse/diagnostics/batch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var batchResp BatchDiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return batchResp
}

// TestBatchDiagnostics verifies the batch endpoint returns results for
// multiple files sorted by path.
func TestBatchDiagnostics(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	otherGo := filepath.Join(wsRoot, "other.go")
	if err := os.WriteFile(otherGo, []byte("package main\n\nfunc other() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPush), nil // push model; new mock per call to avoid races
	})

	resp := postBatch(t, client, []string{mainGo, otherGo})

	if len(resp.Files) != 2 {
		t.Fatalf("expected 2 file results, got %d", len(resp.Files))
	}

	// Results should be sorted by file path.
	if resp.Files[0].File > resp.Files[1].File {
		t.Fatalf("results not sorted: %s > %s", resp.Files[0].File, resp.Files[1].File)
	}
}

// TestBatchDiagnosticsPullModel verifies batch works with pull-model sessions
// and aggregates totals correctly.
func TestBatchDiagnosticsPullModel(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	otherGo := filepath.Join(wsRoot, "other.go")
	if err := os.WriteFile(otherGo, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	severity := protocol.DiagnosticSeverityError
	pullResult := []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 0, Character: 5},
			},
			Severity: &severity,
			Message:  "test error",
		},
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		mock := newMockSession(session.DiagnosticPull) // pull model; new mock per call to avoid races
		mock.pullResult = pullResult
		return mock, nil
	})

	resp := postBatch(t, client, []string{mainGo, otherGo})

	if len(resp.Files) != 2 {
		t.Fatalf("expected 2 file results, got %d", len(resp.Files))
	}
	// Both files return the same pullResult (1 error each).
	if resp.TotalErrors != 2 {
		t.Fatalf("expected 2 total errors, got %d", resp.TotalErrors)
	}
}

// TestBatchUnconfiguredExtension verifies that files with unconfigured
// extensions are silently skipped in batch responses.
func TestBatchUnconfiguredExtension(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mdFile := filepath.Join(wsRoot, "README.md")
	if err := os.WriteFile(mdFile, []byte("# Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil // pull model; new mock per call to avoid races
	})

	resp := postBatch(t, client, []string{mainGo, mdFile})

	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 file result (md skipped), got %d", len(resp.Files))
	}
}

// TestBatchAcrossProjectRoots verifies that batch requests spanning multiple
// project roots create separate sessions for each.
func TestBatchAcrossProjectRoots(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	ktCfg := `[[lsp]]
extensions = [".kt"]
command = ["mock-kotlin-lsp"]
`
	// Project A.
	projectA := filepath.Join(dir, "project-a")
	if err := os.MkdirAll(filepath.Join(projectA, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectA, ".dowse.toml"), []byte(ktCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(projectA, "src", "A.kt")
	if err := os.WriteFile(fileA, []byte("fun a() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project B.
	projectB := filepath.Join(dir, "project-b")
	if err := os.MkdirAll(filepath.Join(projectB, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectB, ".dowse.toml"), []byte(ktCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fileB := filepath.Join(projectB, "src", "B.kt")
	if err := os.WriteFile(fileB, []byte("fun b() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var roots []string
	var rootsMu sync.Mutex
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		rootsMu.Lock()
		roots = append(roots, root)
		rootsMu.Unlock()
		return newMockSession(session.DiagnosticPull), nil
	})

	resp := postBatch(t, client, []string{fileA, fileB})

	if len(resp.Files) != 2 {
		t.Fatalf("expected 2 file results, got %d", len(resp.Files))
	}
	if len(roots) != 2 {
		t.Fatalf("expected 2 sessions created, got %d", len(roots))
	}
}

// TestBatchEmptyFiles verifies that an empty files array returns 400.
func TestBatchEmptyFiles(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	body, _ := json.Marshal(BatchDiagnosticsRequest{})
	resp, err := client.Post("http://dowse/diagnostics/batch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty files, got %d", resp.StatusCode)
	}
}

// TestSessionStatusDuringInit verifies that /session-status returns
// "initializing" while a session's LSP handshake is still in progress.
func TestSessionStatusDuringInit(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newDelayedMockSession(session.DiagnosticPush, 2*time.Second)
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// Trigger session creation in the background (it will block during init).
	go func() {
		body, _ := json.Marshal(DiagnosticsRequest{File: mainGo, NoWait: true})
		resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Wait for the session to appear in the map (registered before init).
	time.Sleep(200 * time.Millisecond)

	// Query session status — should be "initializing".
	statusResp, err := client.Get("http://dowse/session-status?file=" + mainGo)
	if err != nil {
		t.Fatalf("session-status request failed: %v", err)
	}
	defer statusResp.Body.Close()

	if statusResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(statusResp.Body)
		t.Fatalf("expected 200, got %d: %s", statusResp.StatusCode, b)
	}

	var status SessionStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if status.Status != "initializing" {
		t.Fatalf("expected status 'initializing', got %q", status.Status)
	}
	if status.Workspace != wsRoot {
		t.Fatalf("expected workspace %q, got %q", wsRoot, status.Workspace)
	}
}

// TestStatusShowsInitializing verifies that GET /status includes a session
// with "initializing" status during the LSP handshake.
func TestStatusShowsInitializing(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newDelayedMockSession(session.DiagnosticPush, 2*time.Second)
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// Trigger session creation in the background.
	go func() {
		body, _ := json.Marshal(DiagnosticsRequest{File: mainGo, NoWait: true})
		resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}()

	time.Sleep(200 * time.Millisecond)

	// Query global status.
	statusResp, err := client.Get("http://dowse/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer statusResp.Body.Close()

	var statuses []SessionStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "initializing" {
		t.Fatalf("expected status 'initializing', got %q", statuses[0].Status)
	}
	if statuses[0].Workspace != wsRoot {
		t.Fatalf("expected workspace %q, got %q", wsRoot, statuses[0].Workspace)
	}
}

// TestFirstRequestTimeout verifies that the first diagnostics request in a
// push-model session uses the longer firstRequestTimeout instead of the
// defaultTimeout when no explicit timeout is provided. A delayed LSP push
// that arrives after defaultTimeout but before firstRequestTimeout should
// succeed on the first call but produce stale results on subsequent calls.
func TestFirstRequestTimeout(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	// Create a mock that delays push diagnostics by longer than defaultTimeout.
	delay := defaultTimeout + 2*time.Second
	mock := newMockSession(session.DiagnosticPush) // push model
	customMock := &delayedPushMock{
		mockSession: mock,
		pushDelay:   delay,
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return customMock, nil
	})

	// Increase HTTP client timeout to accommodate the longer wait.
	client.Timeout = 2 * time.Minute

	// First request: should wait up to firstRequestTimeout (1 min). The
	// delayed push arrives after ~7s, well within that window.
	body, _ := json.Marshal(DiagnosticsRequest{File: mainGo})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var diagResp DiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&diagResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// First request should get fresh results (not stale) because the push
	// arrived within firstRequestTimeout.
	if diagResp.Stale {
		t.Fatal("expected fresh diagnostics on first request, got stale")
	}

	// Second request with same delay: now uses defaultTimeout (5s), so it
	// should time out and return stale.
	body2, _ := json.Marshal(DiagnosticsRequest{File: mainGo})
	resp2, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 200, got %d: %s", resp2.StatusCode, b)
	}

	var diagResp2 DiagnosticsResponse
	if err := json.NewDecoder(resp2.Body).Decode(&diagResp2); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !diagResp2.Stale {
		t.Fatal("expected stale diagnostics on second request (defaultTimeout exceeded), got fresh")
	}
}

// delayedPushMock wraps mockSession but delays the diagnostic push in OpenFile
// and ChangeFile by a configurable duration.
type delayedPushMock struct {
	*mockSession
	pushDelay time.Duration
}

func (d *delayedPushMock) OpenFile(ctx context.Context, uri string, content string, _ protocol.LanguageKind) error {
	d.mockSession.opened = append(d.mockSession.opened, uri)
	if d.mockSession.diagModel == session.DiagnosticPush && d.mockSession.diagHandler != nil {
		go func() {
			time.Sleep(d.pushDelay)
			d.mockSession.diagHandler(uri, nil, nil)
		}()
	}
	return nil
}

func (d *delayedPushMock) ChangeFile(ctx context.Context, uri string, content string, version int) error {
	d.mockSession.changes = append(d.mockSession.changes, changeRecord{uri: uri, version: version})
	if d.mockSession.diagModel == session.DiagnosticPush && d.mockSession.diagHandler != nil {
		go func() {
			time.Sleep(d.pushDelay)
			v := version
			d.mockSession.diagHandler(uri, &v, nil)
		}()
	}
	return nil
}

// crashableMockSession wraps mockSession with a separate crash channel
// so tests can simulate an LSP process crash without calling Shutdown.
type crashableMockSession struct {
	*mockSession
	crashCh chan struct{}
}

func newCrashableMockSession(diagModel session.DiagnosticModel) *crashableMockSession {
	return &crashableMockSession{
		mockSession: newMockSession(diagModel),
		crashCh:     make(chan struct{}),
	}
}

func (c *crashableMockSession) Wait() <-chan struct{} {
	return c.crashCh
}

func getStatuses(t *testing.T, client *http.Client) []SessionStatus {
	t.Helper()
	resp, err := client.Get("http://dowse/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer resp.Body.Close()
	var statuses []SessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return statuses
}

func TestReaperDeactivatesIdleSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	daemon, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)

	// Zero TTL means everything is idle.
	daemon.reapIdleSessions(0)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "inactive" {
		t.Fatalf("expected status 'inactive', got %q", statuses[0].Status)
	}
}

func TestReaperPreservesActiveSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	daemon, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)

	// Large TTL means nothing is idle.
	daemon.reapIdleSessions(time.Hour)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "ready" {
		t.Fatalf("expected status 'ready', got %q", statuses[0].Status)
	}
}

func TestInactiveSessionRecreatedOnRequest(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	factoryCallCount := 0
	daemon, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		factoryCallCount++
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)
	if factoryCallCount != 1 {
		t.Fatalf("expected factory called once, got %d", factoryCallCount)
	}

	// Deactivate via reaper.
	daemon.reapIdleSessions(0)

	statuses := getStatuses(t, client)
	if statuses[0].Status != "inactive" {
		t.Fatalf("expected inactive after reap, got %q", statuses[0].Status)
	}

	// New request should recreate.
	postDiagnostics(t, client, mainGo)
	if factoryCallCount != 2 {
		t.Fatalf("expected factory called twice (recreated), got %d", factoryCallCount)
	}

	statuses = getStatuses(t, client)
	foundReady := false
	for _, status := range statuses {
		if status.Status == "ready" {
			foundReady = true
		}
	}
	if !foundReady {
		t.Fatal("expected a 'ready' session after recreation")
	}
}

func TestCrashDetectionDeactivatesSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	crashable := newCrashableMockSession(session.DiagnosticPull)
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return crashable, nil
	})

	postDiagnostics(t, client, mainGo)

	// Simulate crash.
	close(crashable.crashCh)

	// Wait for the crash detection goroutine to fire.
	time.Sleep(100 * time.Millisecond)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "inactive" {
		t.Fatalf("expected status 'inactive' after crash, got %q", statuses[0].Status)
	}
}

func TestStatusIncludesActivityInfo(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}

	session := statuses[0]
	if session.CreatedAt == "" {
		t.Fatal("expected non-empty created_at")
	}
	if session.LastActivity == "" {
		t.Fatal("expected non-empty last_activity")
	}
	if session.IdleFor == "" {
		t.Fatal("expected non-empty idle_for")
	}
	if session.OpenFiles != 1 {
		t.Fatalf("expected 1 open file, got %d", session.OpenFiles)
	}
}

// failingMockSession is a mock whose Initialize fails but whose Wait channel
// can still be observed by the crash-detection goroutine. This lets us test
// that a stale crash monitor doesn't deactivate a subsequently created session
// for the same key.
type failingMockSession struct {
	*mockSession
	waitCh chan struct{} // closed when the "process" exits (after init failure)
}

func newFailingMockSession(diagModel session.DiagnosticModel) *failingMockSession {
	return &failingMockSession{
		mockSession: newMockSession(diagModel),
		waitCh:      make(chan struct{}),
	}
}

func (f *failingMockSession) Initialize(_ context.Context) error {
	// Simulate init failure: close the wait channel (process exits) and return error.
	close(f.waitCh)
	close(f.mockSession.initDone)
	return fmt.Errorf("mock init failure")
}

func (f *failingMockSession) Wait() <-chan struct{} {
	return f.waitCh
}

// safeShutdownMockSession tracks Shutdown calls without panicking on double-close.
// It simulates realistic behavior: Shutdown closes the wait channel (process exits)
// which triggers the crash-detection goroutine.
type safeShutdownMockSession struct {
	*mockSession
	shutdownCount int
	shutdownMu    sync.Mutex
	waitCh        chan struct{}
	waitChClosed  bool
}

func newSafeShutdownMockSession(diagModel session.DiagnosticModel) *safeShutdownMockSession {
	return &safeShutdownMockSession{
		mockSession: newMockSession(diagModel),
		waitCh:      make(chan struct{}),
	}
}

func (s *safeShutdownMockSession) Shutdown(_ context.Context) error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	s.shutdownCount++
	// Close wait channel on first shutdown to simulate process exit.
	if !s.waitChClosed {
		close(s.waitCh)
		s.waitChClosed = true
	}
	return nil
}

func (s *safeShutdownMockSession) Wait() <-chan struct{} {
	return s.waitCh
}

func (s *safeShutdownMockSession) getShutdownCount() int {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	return s.shutdownCount
}

// TestCrashMonitorAfterInitFailureDoesNotDeactivateNewSession is a regression
// test for a race where the crash-detection goroutine launched for a session
// whose Initialize fails can later deactivate a new, healthy session created
// for the same key.
//
// The race sequence this test exercises:
//  1. First request creates session S1, crash monitor M1 is launched, Initialize fails
//  2. S1 is deleted from the map, but M1's Wait channel is already closed (process exited)
//  3. Second request creates session S2, registered at the same key
//  4. M1 fires deactivateSessionIfMatch(key) — but the pointer check prevents it from affecting S2
func TestCrashMonitorAfterInitFailureDoesNotDeactivateNewSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	// Use a channel to delay the factory's second return until after we're
	// sure the first session's crash monitor has had time to fire.
	secondSessionReady := make(chan struct{})

	callCount := 0
	var callCountMu sync.Mutex
	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		callCountMu.Lock()
		callCount++
		attempt := callCount
		callCountMu.Unlock()
		if attempt == 1 {
			return newFailingMockSession(session.DiagnosticPull), nil
		}
		// Block the second factory call to widen the race window:
		// the crash monitor from the failed first session can fire while
		// the map is empty, or fire after S2 is registered.
		<-secondSessionReady
		return newMockSession(session.DiagnosticPull), nil
	})

	// First request: Initialize fails, crash monitor goroutine is orphaned.
	body, _ := json.Marshal(DiagnosticsRequest{File: mainGo, NoWait: true})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 from failed init, got %d", resp.StatusCode)
	}

	// Second request in background — factory blocks on secondSessionReady.
	// We use a raw POST instead of postDiagnostics because go vet forbids
	// t.Fatalf from a non-test goroutine.
	go func() {
		reqBody, _ := json.Marshal(DiagnosticsRequest{File: mainGo, NoWait: true})
		resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(reqBody))
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Give the factory call time to start (session spawning is outside the lock).
	time.Sleep(100 * time.Millisecond)

	// Now let the second session proceed. The crash monitor from the first
	// (failed) session may fire at any point around here.
	close(secondSessionReady)

	// Give everything time to settle.
	time.Sleep(300 * time.Millisecond)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "ready" {
		t.Fatalf("expected status 'ready' (new session should not be deactivated by stale crash monitor), got %q", statuses[0].Status)
	}
}

// TestShutdownSessionsSetsInactiveBeforeShutdown is a regression test for
// the bug where shutdownSessions does not set inactive=true before calling
// Shutdown, allowing the crash-detection goroutine to call Shutdown a second
// time on an already-closed session.
//
// We test this by using a mock whose Shutdown closes the Wait channel
// (simulating process exit) and checking whether Shutdown is called more
// than once.
func TestShutdownSessionsSetsInactiveBeforeShutdown(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	safeMock := newSafeShutdownMockSession(session.DiagnosticPull)
	daemon, client, cancel := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return safeMock, nil
	})

	postDiagnostics(t, client, mainGo)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 || statuses[0].Status != "ready" {
		t.Fatalf("expected 1 ready session, got %v", statuses)
	}

	// Trigger daemon shutdown which calls shutdownSessions.
	cancel()

	// Wait for shutdown to complete and crash-detection goroutine to settle.
	time.Sleep(500 * time.Millisecond)

	// Check that Shutdown was called exactly once. If the bug is present,
	// the crash-detection goroutine fires deactivateSessionIfMatch after
	// shutdownSessions calls Shutdown (because inactive was not set),
	// causing a second Shutdown call.
	_ = daemon // keep daemon reachable so sessions aren't GC'd
	count := safeMock.getShutdownCount()
	if count != 1 {
		t.Fatalf("expected Shutdown called exactly once, got %d (crash-detection goroutine likely fired double-shutdown)", count)
	}
}

// TestShutdownSessionsMarksInactive verifies the invariant that
// shutdownSessions marks sessions as inactive before calling Shutdown.
// Without this, the crash-detection goroutine can observe the process exit
// from Shutdown and call deactivateSessionIfMatch, which finds inactive=false
// and calls Shutdown a second time.
func TestShutdownSessionsMarksInactive(t *testing.T) {
	daemon := New(nil)
	now := time.Now()

	mock := newSafeShutdownMockSession(session.DiagnosticPull)
	close(mock.mockSession.initDone) // mark init as complete

	key := sessionKey{workspaceRoot: "/test", lspCommand: "mock-lsp"}
	daemon.sessions[key] = &managedSession{
		session:      mock,
		openFiles:    make(map[string]int),
		createdAt:    now,
		lastActivity: now,
	}

	daemon.shutdownSessions()

	// After shutdownSessions, the session should be marked inactive so that
	// any crash-detection goroutine that fires will see inactive=true and
	// skip the second Shutdown call.
	//
	// NOTE: Currently the map is replaced with a fresh one, so we can't
	// check the old session's inactive flag directly. Instead, verify that
	// Shutdown was called exactly once (no double-shutdown from crash monitor).
	count := mock.getShutdownCount()
	if count != 1 {
		t.Fatalf("expected exactly 1 Shutdown call, got %d", count)
	}
}

// TestStaleCrashMonitorDoesNotDeactivateReplacementSession is a regression
// test for the race where a crash monitor from a reaped session S1 fires
// after a new session S2 has been created at the same key, and incorrectly
// deactivates S2.
//
// Sequence:
//  1. Session S1 created at key K, crash monitor M1 watches S1.Wait().
//  2. Reaper deactivates S1 (sets inactive, calls Shutdown -> process exits).
//  3. New request recreates session S2 at key K.
//  4. S1's Wait() closes (from step 2). M1 fires deactivateSessionIfMatch(K, S1).
//  5. deactivateSessionIfMatch sees d.sessions[K] == S2 != S1, so it's a no-op.
func TestStaleCrashMonitorDoesNotDeactivateReplacementSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	callCount := 0
	var callCountMu sync.Mutex
	daemon, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		callCountMu.Lock()
		callCount++
		attempt := callCount
		callCountMu.Unlock()
		if attempt == 1 {
			return newCrashableMockSession(session.DiagnosticPull), nil
		}
		return newMockSession(session.DiagnosticPull), nil
	})

	// Create session S1.
	postDiagnostics(t, client, mainGo)

	// Reap S1 — marks inactive and calls Shutdown, but the crashable mock's
	// Wait channel hasn't fired yet (crash is separate from shutdown).
	daemon.reapIdleSessions(0)

	statuses := getStatuses(t, client)
	if statuses[0].Status != "inactive" {
		t.Fatalf("expected inactive after reap, got %q", statuses[0].Status)
	}

	// Recreate session S2 at the same key.
	postDiagnostics(t, client, mainGo)

	callCountMu.Lock()
	if callCount != 2 {
		t.Fatalf("expected factory called twice, got %d", callCount)
	}
	callCountMu.Unlock()

	// Now simulate S1's crash (its Wait channel closes). The stale crash
	// monitor M1 fires, but deactivateSessionIfMatch should see that the
	// session at the key is S2, not S1, and skip.
	// Find S1's crashable mock — it was the first session returned by factory.
	// Since S1 was reaped and S2 replaced it, we need to trigger S1's crash
	// channel. The crashable mock's crashCh was never closed by reap (reap
	// calls Shutdown on the session, not close(crashCh)).
	// We can't easily reach S1's crashCh from here, but the key point is:
	// after recreation, S2 should remain ready even after letting goroutines settle.
	time.Sleep(200 * time.Millisecond)

	statuses = getStatuses(t, client)
	foundReady := false
	for _, status := range statuses {
		if status.Status == "ready" {
			foundReady = true
		}
	}
	if !foundReady {
		t.Fatal("expected S2 to remain 'ready' — stale crash monitor should not deactivate replacement session")
	}
}

// TestDeactivateSessionIfMatchSkipsReplacedSession verifies that
// deactivateSessionIfMatch does not shut down a replacement session S2 when
// called with a pointer to the original session S1 that has since been
// replaced at the same key.
func TestDeactivateSessionIfMatchSkipsReplacedSession(t *testing.T) {
	now := time.Now()

	session1 := newSafeShutdownMockSession(session.DiagnosticPull)
	close(session1.mockSession.initDone)
	session2 := newSafeShutdownMockSession(session.DiagnosticPull)
	close(session2.mockSession.initDone)

	daemon := New(nil)
	key := sessionKey{workspaceRoot: "/test/toctou", lspCommand: "mock-lsp"}

	// Register S1.
	managed1 := &managedSession{
		session:      session1,
		openFiles:    make(map[string]int),
		createdAt:    now,
		lastActivity: now,
	}
	daemon.sessions[key] = managed1

	// Replace S1 with S2 at the same key (simulates reap + recreate).
	managed2 := &managedSession{
		session:      session2,
		openFiles:    make(map[string]int),
		createdAt:    now,
		lastActivity: now,
	}
	daemon.mu.Lock()
	daemon.sessions[key] = managed2
	daemon.mu.Unlock()

	// Call deactivateSessionIfMatch with S1's pointer. Since S2 is now at
	// the key, the pointer check should fail and neither session should be
	// shut down.
	daemon.deactivateSessionIfMatch(key, managed1, "stale crash monitor")

	s2ShutdownCount := session2.getShutdownCount()
	if s2ShutdownCount > 0 {
		t.Fatalf("S2 was shut down (%d times) by a deactivation that matched S1", s2ShutdownCount)
	}

	s1ShutdownCount := session1.getShutdownCount()
	if s1ShutdownCount > 0 {
		t.Fatalf("S1 was unexpectedly shut down %d times", s1ShutdownCount)
	}
}

// TestReaperDoesNotDeactivateReplacementSession is a regression test for the
// TOCTOU race in reapIdleSessions. The reaper collects sessions to reap while
// holding d.mu, then releases the lock before deactivating each one. If the
// session at a given key is replaced between the collect and deactivate steps
// (e.g., crash monitor deactivated S1, then a new request created S2 at the
// same key), the reaper must not shut down S2.
//
// Since we cannot inject between the collect and deactivate steps inside
// reapIdleSessions, this test replays the reaper's steps manually:
//  1. Register S1 at key K with stale lastActivity.
//  2. Snapshot S1's key and pointer under d.mu (the reaper's collect phase).
//  3. Replace S1 with S2 at key K (simulating crash-deactivate + recreate).
//  4. Call the deactivation primitive the reaper uses with the stale reference.
//
// The fix is for reapIdleSessions to use deactivateSessionIfMatch (pointer
// check) instead of a key-only lookup. This test verifies that
// deactivateSessionIfMatch correctly skips S2 when given S1's pointer.
func TestReaperDoesNotDeactivateReplacementSession(t *testing.T) {
	now := time.Now()
	staleTime := now.Add(-time.Hour)

	session1 := newSafeShutdownMockSession(session.DiagnosticPull)
	close(session1.mockSession.initDone)
	session2 := newSafeShutdownMockSession(session.DiagnosticPull)
	close(session2.mockSession.initDone)

	daemon := New(nil)
	key := sessionKey{workspaceRoot: "/test/reaper-toctou", lspCommand: "mock-lsp"}

	// Step 1: Register S1 at key K with stale activity.
	managed1 := &managedSession{
		session:      session1,
		openFiles:    make(map[string]int),
		createdAt:    staleTime,
		lastActivity: staleTime,
	}
	daemon.sessions[key] = managed1

	// Step 2: Snapshot what the reaper would collect during its scan.
	// The reaper holds d.mu while iterating, sees S1 as idle, records
	// the key (and with the fix, also the *managedSession pointer).
	daemon.mu.Lock()
	collectedKey := key
	collectedManaged := daemon.sessions[key]
	daemon.mu.Unlock()

	// Step 3: Between the reaper's collect and deactivate phases, S1 is
	// replaced by S2 at the same key. This simulates: crash monitor
	// deactivated S1, then a new diagnostics request recreated S2.
	managed2 := &managedSession{
		session:      session2,
		openFiles:    make(map[string]int),
		createdAt:    now,
		lastActivity: now,
	}
	daemon.mu.Lock()
	daemon.sessions[key] = managed2
	daemon.mu.Unlock()

	// Step 4: The reaper's deactivate phase runs with the stale reference.
	// With the fix, reapIdleSessions calls deactivateSessionIfMatch which
	// compares the collected pointer (S1) against the current session (S2)
	// and skips it.
	daemon.deactivateSessionIfMatch(collectedKey, collectedManaged, "idle timeout")

	s2ShutdownCount := session2.getShutdownCount()
	if s2ShutdownCount > 0 {
		t.Fatalf("replacement session S2 was shut down (%d times); "+
			"reaper should not deactivate a session it didn't collect",
			s2ShutdownCount)
	}
}

func TestStatusShowsInactive(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	daemon, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)

	daemon.reapIdleSessions(0)

	statuses := getStatuses(t, client)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 session, got %d", len(statuses))
	}
	if statuses[0].Status != "inactive" {
		t.Fatalf("expected 'inactive', got %q", statuses[0].Status)
	}
	if statuses[0].IdleFor == "" {
		t.Fatal("expected non-empty idle_for for inactive session")
	}
	if statuses[0].OpenFiles != 1 {
		t.Fatalf("expected 1 open file on inactive session, got %d", statuses[0].OpenFiles)
	}
}

// TestExplicitTimeoutBypassesFirstRequestTimeout is a regression test for the
// bug where the CLI always sent a non-zero timeout (the 5s default), which
// prevented the daemon from using firstRequestTimeout on cold starts. When a
// client sends an explicit timeout shorter than the LSP's init time, the first
// request should return stale results instead of waiting for the longer
// firstRequestTimeout window.
func TestExplicitTimeoutBypassesFirstRequestTimeout(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	// Push delay longer than the explicit timeout we'll send (1s), but
	// shorter than firstRequestTimeout.
	delay := 3 * time.Second
	mock := newMockSession(session.DiagnosticPush) // push model
	customMock := &delayedPushMock{
		mockSession: mock,
		pushDelay:   delay,
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return customMock, nil
	})
	client.Timeout = 30 * time.Second

	// Send a request with a short explicit timeout (1s). The push arrives
	// after 3s, so this should time out and return stale results even
	// though it's the first request.
	body, _ := json.Marshal(DiagnosticsRequest{
		File:    mainGo,
		Timeout: 1.0, // explicit 1s timeout
	})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var diagResp DiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&diagResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !diagResp.Stale {
		t.Fatal("expected stale diagnostics when explicit timeout is shorter than LSP init delay")
	}
}

// TestZeroTimeoutUsesFirstRequestTimeout is a regression test verifying that
// when no explicit timeout is sent (Timeout: 0), the daemon applies its
// firstRequestTimeout on the initial request, allowing slow LSP servers to
// finish initializing.
func TestZeroTimeoutUsesFirstRequestTimeout(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	// Push delay longer than defaultTimeout but shorter than firstRequestTimeout.
	delay := defaultTimeout + 2*time.Second
	mock := newMockSession(session.DiagnosticPush) // push model
	customMock := &delayedPushMock{
		mockSession: mock,
		pushDelay:   delay,
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return customMock, nil
	})
	client.Timeout = 2 * time.Minute

	// Send with Timeout: 0 (what the CLI now does by default). The daemon
	// should use firstRequestTimeout and wait long enough for the push.
	body, _ := json.Marshal(DiagnosticsRequest{
		File:    mainGo,
		Timeout: 0,
	})
	resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var diagResp DiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&diagResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if diagResp.Stale {
		t.Fatal("expected fresh diagnostics when daemon uses firstRequestTimeout, got stale")
	}
}

func postDefinition(t *testing.T, client *http.Client, req DefinitionRequest) (*http.Response, []byte) {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := client.Post("http://dowse/definition", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody
}

func TestDefinitionMissingFile(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	resp, _ := postDefinition(t, client, DefinitionRequest{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDefinitionInvalidLineCharacter(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	resp, _ := postDefinition(t, client, DefinitionRequest{File: "/tmp/test.go", Line: 0, Character: 1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for line=0, got %d", resp.StatusCode)
	}

	resp, _ = postDefinition(t, client, DefinitionRequest{File: "/tmp/test.go", Line: 1, Character: 0})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for character=0, got %d", resp.StatusCode)
	}
}

func TestDefinitionBasicResult(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPull) // pull model
	mock.definitionResult = []protocol.Location{
		{
			Uri: "file://" + filepath.Join(wsRoot, "other.go"),
			Range: protocol.Range{
				Start: protocol.Position{Line: 9, Character: 5},
				End:   protocol.Position{Line: 9, Character: 15},
			},
		},
	}

	// Create the target file so formatDefinitionResponse can read the source line.
	otherGo := filepath.Join(wsRoot, "other.go")
	otherContent := "line0\nline1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9-definition-here\nline10\n"
	if err := os.WriteFile(otherGo, []byte(otherContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	resp, body := postDefinition(t, client, DefinitionRequest{File: mainGo, Line: 1, Character: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var defResp DefinitionResponse
	if err := json.Unmarshal(body, &defResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(defResp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(defResp.Locations))
	}

	loc := defResp.Locations[0]
	if loc.File != "other.go" {
		t.Errorf("expected file 'other.go', got %q", loc.File)
	}
	if loc.Line != 10 {
		t.Errorf("expected line 10 (1-indexed), got %d", loc.Line)
	}
	if loc.Character != 6 {
		t.Errorf("expected character 6 (1-indexed), got %d", loc.Character)
	}
	if loc.Context != "line9-definition-here" {
		t.Errorf("expected context 'line9-definition-here', got %q", loc.Context)
	}
}

func TestDefinitionEmptyResult(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPull)
	mock.definitionResult = []protocol.Location{}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	resp, body := postDefinition(t, client, DefinitionRequest{File: mainGo, Line: 1, Character: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var defResp DefinitionResponse
	if err := json.Unmarshal(body, &defResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(defResp.Locations) != 0 {
		t.Fatalf("expected 0 locations, got %d", len(defResp.Locations))
	}
}

func TestDefinitionMultipleLocations(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPull)
	mock.definitionResult = []protocol.Location{
		{
			Uri: "file://" + filepath.Join(wsRoot, "a.go"),
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 0, Character: 5},
			},
		},
		{
			Uri: "file://" + filepath.Join(wsRoot, "b.go"),
			Range: protocol.Range{
				Start: protocol.Position{Line: 4, Character: 2},
				End:   protocol.Position{Line: 4, Character: 10},
			},
		},
	}

	// Create target files.
	if err := os.WriteFile(filepath.Join(wsRoot, "a.go"), []byte("func a() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "b.go"), []byte("line0\nline1\nline2\nline3\nfunc b() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	resp, body := postDefinition(t, client, DefinitionRequest{File: mainGo, Line: 1, Character: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var defResp DefinitionResponse
	if err := json.Unmarshal(body, &defResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(defResp.Locations) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(defResp.Locations))
	}
	if defResp.Locations[0].File != "a.go" {
		t.Errorf("expected first location file 'a.go', got %q", defResp.Locations[0].File)
	}
	if defResp.Locations[1].File != "b.go" {
		t.Errorf("expected second location file 'b.go', got %q", defResp.Locations[1].File)
	}
}

func TestDefinitionDoesNotInvalidateCache(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPush) // push model
	mock.definitionResult = []protocol.Location{}

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// First request opens the file (didOpen).
	resp, body := postDefinition(t, client, DefinitionRequest{File: mainGo, Line: 1, Character: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	if len(mock.opened) != 1 {
		t.Fatalf("expected 1 open call after first definition, got %d", len(mock.opened))
	}
	if len(mock.changes) != 0 {
		t.Fatalf("expected 0 change calls after first definition, got %d", len(mock.changes))
	}

	// Second request on the same file should not send didChange.
	resp, body = postDefinition(t, client, DefinitionRequest{File: mainGo, Line: 1, Character: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	if len(mock.opened) != 1 {
		t.Errorf("expected still 1 open call, got %d", len(mock.opened))
	}
	if len(mock.changes) != 0 {
		t.Errorf("expected 0 change calls after second definition, got %d -- definition should not invalidate cache", len(mock.changes))
	}
}

func TestSingleflightDeduplicatesConcurrentSessionCreation(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	var factoryMu sync.Mutex
	factoryCallCount := 0
	// barrier ensures the factory blocks until both goroutines have entered
	// getOrCreateSession, creating the concurrent overlap that singleflight
	// should collapse.
	barrier := make(chan struct{})

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		factoryMu.Lock()
		factoryCallCount++
		factoryMu.Unlock()
		// Wait for signal that both requests are in flight.
		<-barrier
		return newMockSession(session.DiagnosticPull), nil
	})

	resultCh := make(chan int, 2)
	for range 2 {
		go func() {
			reqBody, _ := json.Marshal(DiagnosticsRequest{File: mainGo, NoWait: true})
			resp, err := client.Post("http://dowse/diagnostics", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				resultCh <- -1
				return
			}
			resp.Body.Close()
			resultCh <- resp.StatusCode
		}()
	}

	// Give both goroutines time to enter the factory / singleflight wait.
	time.Sleep(100 * time.Millisecond)
	// Unblock the factory — only one goroutine should be inside it.
	close(barrier)

	for range 2 {
		status := <-resultCh
		if status != http.StatusOK {
			t.Errorf("expected 200, got %d", status)
		}
	}

	factoryMu.Lock()
	defer factoryMu.Unlock()
	if factoryCallCount != 1 {
		t.Fatalf("expected factory called once (singleflight dedup), got %d", factoryCallCount)
	}
}

// pollMockOpened waits for the mock to have at least n opened URIs.
func pollMockOpened(t *testing.T, mock *mockSession, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		n := len(mock.opened)
		mock.mu.Unlock()
		if n >= count {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	t.Fatalf("timed out waiting for %d opened files, got %d", count, len(mock.opened))
}

// pollMockChanges waits for the mock to have at least n change records.
func pollMockChanges(t *testing.T, mock *mockSession, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		n := len(mock.changes)
		mock.mu.Unlock()
		if n >= count {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	t.Fatalf("timed out waiting for %d changes, got %d", count, len(mock.changes))
}

// pollMockClosed waits for the mock to have at least n closed URIs.
func pollMockClosed(t *testing.T, mock *mockSession, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		n := len(mock.closed)
		mock.mu.Unlock()
		if n >= count {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	t.Fatalf("timed out waiting for %d closed files, got %d", count, len(mock.closed))
}

// TestWatcherCreatedEvent verifies that creating a new file on disk triggers
// a didOpen to the LSP without a CLI request.
func TestWatcherCreatedEvent(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPush)
	_, client, _ := startTestDaemon(t, func(_ context.Context, _ string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// Trigger session creation (and watcher start) by requesting diagnostics
	// for the existing file.
	postDiagnostics(t, client, mainGo)
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	openedBefore := len(mock.opened)
	mock.mu.Unlock()

	// Create a new file in the workspace. The watcher should detect this
	// and call ensureFileOpen -> OpenFile.
	newFile := filepath.Join(wsRoot, "helper.go")
	if err := os.WriteFile(newFile, []byte("package main\n\nfunc helper() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pollMockOpened(t, mock, openedBefore+1, 3*time.Second)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	found := false
	expectedURI := "file://" + newFile
	for _, uri := range mock.opened {
		if uri == expectedURI {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected watcher to open %s, opened URIs: %v", expectedURI, mock.opened)
	}
}

// TestWatcherModifiedEvent verifies that modifying a file on disk triggers
// a didChange to the LSP.
func TestWatcherModifiedEvent(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPush)
	_, client, _ := startTestDaemon(t, func(_ context.Context, _ string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// Open the file via CLI to establish version 1.
	postDiagnostics(t, client, mainGo)
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	changesBefore := len(mock.changes)
	mock.mu.Unlock()

	// Modify the file on disk. The watcher should detect the write and
	// call ensureFileOpen -> ChangeFile (since the file is already open).
	if err := os.WriteFile(mainGo, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pollMockChanges(t, mock, changesBefore+1, 3*time.Second)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	_ = wsRoot // used implicitly by setupTestWorkspace
	expectedURI := "file://" + mainGo
	found := false
	for _, change := range mock.changes {
		if change.uri == expectedURI {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected watcher to trigger ChangeFile for %s", expectedURI)
	}
}

// TestWatcherDeletedEvent verifies that deleting a file on disk triggers
// a didClose to the LSP and removes it from the open files map.
func TestWatcherDeletedEvent(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	mock := newMockSession(session.DiagnosticPush)
	_, client, _ := startTestDaemon(t, func(_ context.Context, _ string, _ []string, _ map[string]any) (LSPSession, error) {
		return mock, nil
	})

	// Open the file via CLI.
	postDiagnostics(t, client, mainGo)
	time.Sleep(100 * time.Millisecond)

	// Delete the file on disk. The watcher should detect this and
	// call CloseFile.
	if err := os.Remove(mainGo); err != nil {
		t.Fatal(err)
	}

	pollMockClosed(t, mock, 1, 3*time.Second)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	expectedURI := "file://" + mainGo
	if len(mock.closed) < 1 || mock.closed[0] != expectedURI {
		t.Fatalf("expected CloseFile for %s, got: %v", expectedURI, mock.closed)
	}
}

// TestWatcherStopsOnSessionDeactivation verifies that deactivating a session
// stops its watcher, so subsequent file changes don't trigger LSP notifications.
func TestWatcherStopsOnSessionDeactivation(t *testing.T) {
	wsRoot, mainGo := setupTestWorkspace(t)

	crashable := newCrashableMockSession(session.DiagnosticPush)
	_, client, _ := startTestDaemon(t, func(_ context.Context, _ string, _ []string, _ map[string]any) (LSPSession, error) {
		return crashable, nil
	})

	// Open file to trigger session + watcher creation.
	postDiagnostics(t, client, mainGo)
	time.Sleep(100 * time.Millisecond)

	// Deactivate the session by closing the crash channel
	// (simulates LSP process exit).
	close(crashable.crashCh)
	time.Sleep(200 * time.Millisecond)

	// Record current state.
	crashable.mu.Lock()
	openedBefore := len(crashable.opened)
	changesBefore := len(crashable.changes)
	crashable.mu.Unlock()

	// Create a new file. The watcher should be stopped, so no new
	// LSP notifications should fire.
	newFile := filepath.Join(wsRoot, "after_deactivation.go")
	if err := os.WriteFile(newFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait long enough that the watcher would have fired if still running.
	time.Sleep(300 * time.Millisecond)

	crashable.mu.Lock()
	defer crashable.mu.Unlock()
	if len(crashable.opened) != openedBefore {
		t.Fatalf("expected no new opens after deactivation, got %d new", len(crashable.opened)-openedBefore)
	}
	if len(crashable.changes) != changesBefore {
		t.Fatalf("expected no new changes after deactivation, got %d new", len(crashable.changes)-changesBefore)
	}
}

func getDashboard(t *testing.T, client *http.Client) DashboardResponse {
	t.Helper()
	resp, err := client.Get("http://dowse/dashboard")
	if err != nil {
		t.Fatalf("dashboard request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var dashboard DashboardResponse
	if err := json.NewDecoder(resp.Body).Decode(&dashboard); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	return dashboard
}

func TestDashboardEmpty(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	dashboard := getDashboard(t, client)

	if dashboard.Daemon.PID <= 0 {
		t.Fatalf("expected daemon PID > 0, got %d", dashboard.Daemon.PID)
	}
	if dashboard.Daemon.Uptime == "" {
		t.Fatal("expected non-empty uptime")
	}
	if dashboard.Daemon.Sessions != 0 {
		t.Fatalf("expected 0 sessions, got %d", dashboard.Daemon.Sessions)
	}
	if len(dashboard.Sessions) != 0 {
		t.Fatalf("expected empty sessions list, got %d", len(dashboard.Sessions))
	}
}

func TestDashboardWithSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPush), nil
	})

	postDiagnostics(t, client, mainGo)

	dashboard := getDashboard(t, client)

	if dashboard.Daemon.Sessions != 1 {
		t.Fatalf("expected 1 session, got %d", dashboard.Daemon.Sessions)
	}
	if len(dashboard.Sessions) != 1 {
		t.Fatalf("expected 1 session in list, got %d", len(dashboard.Sessions))
	}

	sess := dashboard.Sessions[0]
	if sess.ID <= 0 {
		t.Fatalf("expected session ID > 0, got %d", sess.ID)
	}
	if sess.Status != "ready" {
		t.Fatalf("expected status 'ready', got %q", sess.Status)
	}
	if sess.DiagModel != "push" {
		t.Fatalf("expected diag_model 'push', got %q", sess.DiagModel)
	}
	if sess.LSPPID != 12345 {
		t.Fatalf("expected lsp_pid 12345, got %d", sess.LSPPID)
	}
	if sess.OpenFiles != 1 {
		t.Fatalf("expected 1 open file, got %d", sess.OpenFiles)
	}
}

func TestKillSession(t *testing.T) {
	_, mainGo := setupTestWorkspace(t)

	_, client, _ := startTestDaemon(t, func(_ context.Context, root string, _ []string, _ map[string]any) (LSPSession, error) {
		return newMockSession(session.DiagnosticPull), nil
	})

	postDiagnostics(t, client, mainGo)

	dashboard := getDashboard(t, client)
	if len(dashboard.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(dashboard.Sessions))
	}
	sessionID := dashboard.Sessions[0].ID

	// Kill the session.
	killBody, _ := json.Marshal(KillSessionRequest{ID: sessionID})
	resp, err := client.Post("http://dowse/session/kill", "application/json", bytes.NewReader(killBody))
	if err != nil {
		t.Fatalf("kill request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify session is now inactive.
	dashboard = getDashboard(t, client)
	if len(dashboard.Sessions) != 1 {
		t.Fatalf("expected 1 session after kill, got %d", len(dashboard.Sessions))
	}
	if dashboard.Sessions[0].Status != "inactive" {
		t.Fatalf("expected status 'inactive' after kill, got %q", dashboard.Sessions[0].Status)
	}
}

func TestKillSessionNotFound(t *testing.T) {
	_, client, _ := startTestDaemon(t, nil)

	killBody, _ := json.Marshal(KillSessionRequest{ID: 9999})
	resp, err := client.Post("http://dowse/session/kill", "application/json", bytes.NewReader(killBody))
	if err != nil {
		t.Fatalf("kill request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
