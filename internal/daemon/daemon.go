// Package daemon implements the background daemon process that listens on a
// Unix domain socket and routes requests to the appropriate LSP sessions.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dreammify/dowse/internal/cache"
	"github.com/dreammify/dowse/internal/config"
	"github.com/dreammify/dowse/internal/lsp/protocol"
	"github.com/dreammify/dowse/internal/session"
	"github.com/dreammify/dowse/internal/watcher"
	"golang.org/x/sync/singleflight"
)

// LSPSession defines the interface for an LSP session. The concrete
// implementation lives in the session package; this interface decouples the
// daemon from it for testability.
type LSPSession interface {
	Initialize(ctx context.Context) error
	WaitReady(ctx context.Context) error
	OpenFile(ctx context.Context, uri string, content string, languageID protocol.LanguageKind) error
	ChangeFile(ctx context.Context, uri string, content string, version int) error
	CloseFile(ctx context.Context, uri string) error
	PullDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error)
	Definition(ctx context.Context, uri string, line uint32, character uint32) ([]protocol.Location, error)
	DiagModel() session.DiagnosticModel
	PID() int
	OnDiagnostics(handler func(uri string, version *int, diagnostics []protocol.Diagnostic))
	Progress() string
	ProgressWithPercent() (string, *uint32)
	Shutdown(ctx context.Context) error
	Wait() <-chan struct{}
}

// SessionFactory creates new LSP sessions. The daemon calls this on the first
// request for a workspace+LSP combination.
type SessionFactory func(ctx context.Context, workspaceRoot string, lspCmd []string, initOptions map[string]any) (LSPSession, error)

// sessionKey identifies a unique session (workspace root + LSP command).
type sessionKey struct {
	workspaceRoot string
	lspCommand    string // joined command slice for map key
}

type managedSession struct {
	id           int
	session      LSPSession
	cache        *cache.Cache
	mu           sync.Mutex
	openFiles    map[string]int         // uri -> latest version sent
	fileLocks    map[string]*sync.Mutex // per-URI lock for file operations
	firstRequest bool                   // true until first diagnostics request completes
	createdAt    time.Time
	lastActivity time.Time
	inactive     bool               // true = LSP shut down, kept as tombstone for status visibility
	cancelCtx    context.CancelFunc // cancels per-session context; stops watcher + crash monitor
	extensions   []string           // file extensions this session handles (e.g., [".go"])
}

// fileLock returns a per-URI mutex, creating one if needed. Must be called
// with managed.mu held (briefly, just to fetch/create the lock).
func (m *managedSession) fileLock(uri string) *sync.Mutex {
	if m.fileLocks == nil {
		m.fileLocks = make(map[string]*sync.Mutex)
	}
	lock, ok := m.fileLocks[uri]
	if !ok {
		lock = &sync.Mutex{}
		m.fileLocks[uri] = lock
	}
	return lock
}

// DiagnosticsRequest is the JSON body for POST /diagnostics.
type DiagnosticsRequest struct {
	File    string  `json:"file"`
	NoWait  bool    `json:"no_wait"`
	Timeout float64 `json:"timeout,omitempty"` // seconds
}

const defaultTimeout = 5 * time.Second
const firstRequestTimeout = 1 * time.Minute

// DiagnosticsResponse is the JSON body returned from POST /diagnostics.
type DiagnosticsResponse struct {
	File         string               `json:"file"`
	Diagnostics  []EnrichedDiagnostic `json:"diagnostics"`
	Stale        bool                 `json:"stale"`
	ErrorCount   int                  `json:"error_count"`
	WarningCount int                  `json:"warning_count"`
}

// errUnconfiguredExtension is returned when a file's extension has no
// configured LSP. The batch handler uses this to silently skip files.
var errUnconfiguredExtension = errors.New("unconfigured extension")

// BatchDiagnosticsRequest is the JSON body for POST /diagnostics/batch.
type BatchDiagnosticsRequest struct {
	Files   []string `json:"files"`
	NoWait  bool     `json:"no_wait"`
	Timeout float64  `json:"timeout,omitempty"` // seconds
}

// BatchDiagnosticsResponse is the JSON body returned from POST /diagnostics/batch.
type BatchDiagnosticsResponse struct {
	Files         []DiagnosticsResponse `json:"files"`
	TotalErrors   int                   `json:"total_errors"`
	TotalWarnings int                   `json:"total_warnings"`
}

// DefinitionRequest is the JSON body for POST /definition.
type DefinitionRequest struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

// DefinitionResponse is the JSON body returned from POST /definition.
type DefinitionResponse struct {
	Locations []DefinitionLocation `json:"locations"`
}

// SessionStatus is returned by the /status and /session-status endpoints.
type SessionStatus struct {
	Workspace    string  `json:"workspace"`
	LSP          string  `json:"lsp"`
	Status       string  `json:"status"`
	Percent      *uint32 `json:"percent,omitempty"`
	CreatedAt    string  `json:"created_at"`
	LastActivity string  `json:"last_activity"`
	IdleFor      string  `json:"idle_for"`
	OpenFiles    int     `json:"open_files"`
}

// DaemonInfo contains daemon-level metadata for the dashboard.
type DaemonInfo struct {
	PID      int    `json:"pid"`
	Uptime   string `json:"uptime"`
	Sessions int    `json:"session_count"`
}

// DashboardSession contains per-session data for the dashboard.
type DashboardSession struct {
	ID          int     `json:"id"`
	Workspace   string  `json:"workspace"`
	LSP         string  `json:"lsp"`
	LSPPID      int     `json:"lsp_pid"`
	Status      string  `json:"status"`
	Percent     *uint32 `json:"percent,omitempty"`
	DiagModel   string  `json:"diag_model"`
	OpenFiles   int     `json:"open_files"`
	CachedFiles int     `json:"cached_files"`
	TotalDiags  int     `json:"total_diagnostics"`
	IdleFor     string  `json:"idle_for"`
}

// DashboardResponse is the JSON body returned from GET /dashboard.
type DashboardResponse struct {
	Daemon   DaemonInfo         `json:"daemon"`
	Sessions []DashboardSession `json:"sessions"`
}

// KillSessionRequest is the JSON body for POST /session/kill.
type KillSessionRequest struct {
	ID int `json:"id"`
}

// Daemon manages LSP sessions and serves an HTTP API over a Unix socket.
type Daemon struct {
	factory      SessionFactory
	mu           sync.Mutex
	sessions     map[sessionKey]*managedSession
	cancel       context.CancelFunc
	sessionGroup singleflight.Group
	startedAt    time.Time
	nextID       int
}

// New creates a Daemon with the given session factory.
func New(factory SessionFactory) *Daemon {
	return &Daemon{
		factory:  factory,
		sessions: make(map[sessionKey]*managedSession),
	}
}

// Run starts the daemon, listening on the Unix socket at socketPath.
// It blocks until ctx is cancelled, then performs a graceful shutdown.
func (d *Daemon) Run(ctx context.Context, socketPath string) error {
	d.startedAt = time.Now()
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	defer cancel()

	// Remove stale socket file.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}

	// Ensure the directory exists.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", d.handlePing)
	mux.HandleFunc("POST /diagnostics", d.handleDiagnostics(ctx))
	mux.HandleFunc("POST /diagnostics/batch", d.handleBatchDiagnostics(ctx))
	mux.HandleFunc("POST /definition", d.handleDefinition(ctx))
	mux.HandleFunc("POST /shutdown", d.handleShutdown)
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /session-status", d.handleSessionStatus(ctx))
	mux.HandleFunc("GET /dashboard", d.handleDashboard)
	mux.HandleFunc("POST /session/kill", d.handleSessionKill)

	srv := &http.Server{Handler: mux}

	sessionTTL := d.loadSessionTTL()
	if sessionTTL > 0 {
		d.startReaper(ctx, sessionTTL)
		slog.Info("session reaper started", "ttl", sessionTTL)
	}

	// Shut down when context is cancelled.
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("daemon listening", "socket", socketPath)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serving: %w", err)
	}

	d.shutdownSessions()
	return nil
}

func (d *Daemon) handlePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("shutting down"))
	// Cancel the daemon context after responding.
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	statuses := make([]SessionStatus, 0, len(d.sessions))
	for key, managed := range d.sessions {
		managed.mu.Lock()
		isInactive := managed.inactive
		createdAt := managed.createdAt
		lastActivity := managed.lastActivity
		openFileCount := len(managed.openFiles)
		managed.mu.Unlock()

		var status string
		var percent *uint32
		if isInactive {
			status = "inactive"
		} else {
			status, percent = managed.session.ProgressWithPercent()
			if status == "" {
				status = "ready"
			}
		}

		statuses = append(statuses, SessionStatus{
			Workspace:    key.workspaceRoot,
			LSP:          strings.ReplaceAll(key.lspCommand, "\x00", " "),
			Status:       status,
			Percent:      percent,
			CreatedAt:    createdAt.Format(time.RFC3339),
			LastActivity: lastActivity.Format(time.RFC3339),
			IdleFor:      time.Since(lastActivity).Truncate(time.Second).String(),
			OpenFiles:    openFileCount,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(statuses); err != nil {
		slog.Debug("failed to write response", "err", err)
	}
}

func (d *Daemon) handleSessionStatus(dCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filePath := r.URL.Query().Get("file")
		if filePath == "" {
			http.Error(w, "file query parameter is required", http.StatusBadRequest)
			return
		}

		absPath, err := filepath.Abs(filePath)
		if err != nil {
			http.Error(w, fmt.Sprintf("resolving path: %v", err), http.StatusBadRequest)
			return
		}
		if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
			absPath = resolved
		}

		gitRootDir, err := gitRoot(dCtx, absPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("finding workspace root: %v", err), http.StatusBadRequest)
			return
		}

		result, err := config.FindNearest(absPath, gitRootDir, globalConfigPath())
		if err != nil {
			http.Error(w, fmt.Sprintf("loading config: %v", err), http.StatusInternalServerError)
			return
		}

		ext := filepath.Ext(absPath)
		lspCfg, err := result.Config.Resolve(ext)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		key := sessionKey{
			workspaceRoot: result.ProjectRoot,
			lspCommand:    strings.Join(lspCfg.Command, "\x00"),
		}

		d.mu.Lock()
		managed, exists := d.sessions[key]
		d.mu.Unlock()

		if !exists {
			http.Error(w, "no session for this file", http.StatusNotFound)
			return
		}

		managed.mu.Lock()
		isInactive := managed.inactive
		createdAt := managed.createdAt
		lastActivity := managed.lastActivity
		openFileCount := len(managed.openFiles)
		managed.mu.Unlock()

		var status string
		var percent *uint32
		if isInactive {
			status = "inactive"
		} else {
			status, percent = managed.session.ProgressWithPercent()
			if status == "" {
				status = "ready"
			}
		}

		resp := SessionStatus{
			Workspace:    key.workspaceRoot,
			LSP:          strings.ReplaceAll(key.lspCommand, "\x00", " "),
			Status:       status,
			Percent:      percent,
			CreatedAt:    createdAt.Format(time.RFC3339),
			LastActivity: lastActivity.Format(time.RFC3339),
			IdleFor:      time.Since(lastActivity).Truncate(time.Second).String(),
			OpenFiles:    openFileCount,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Debug("failed to write response", "err", err)
		}
	}
}

func (d *Daemon) handleDashboard(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	sessions := make([]DashboardSession, 0, len(d.sessions))
	for key, managed := range d.sessions {
		managed.mu.Lock()
		sessionID := managed.id
		isInactive := managed.inactive
		openFileCount := len(managed.openFiles)
		lastActivity := managed.lastActivity
		managed.mu.Unlock()

		var status string
		var percent *uint32
		var lspPID int
		var diagModelStr string
		if isInactive {
			status = "inactive"
			diagModelStr = "unknown"
		} else {
			status, percent = managed.session.ProgressWithPercent()
			if status == "" {
				status = "ready"
			}
			lspPID = managed.session.PID()
			if managed.session.DiagModel() == session.DiagnosticPull {
				diagModelStr = "pull"
			} else {
				diagModelStr = "push"
			}
		}

		cacheStats := managed.cache.Stats()

		sessions = append(sessions, DashboardSession{
			ID:          sessionID,
			Workspace:   key.workspaceRoot,
			LSP:         strings.ReplaceAll(key.lspCommand, "\x00", " "),
			LSPPID:      lspPID,
			Status:      status,
			Percent:     percent,
			DiagModel:   diagModelStr,
			OpenFiles:   openFileCount,
			CachedFiles: cacheStats.TotalFiles,
			TotalDiags:  cacheStats.TotalDiags,
			IdleFor:     time.Since(lastActivity).Truncate(time.Second).String(),
		})
	}
	d.mu.Unlock()

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	resp := DashboardResponse{
		Daemon: DaemonInfo{
			PID:      os.Getpid(),
			Uptime:   time.Since(d.startedAt).Truncate(time.Second).String(),
			Sessions: len(sessions),
		},
		Sessions: sessions,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Debug("failed to write response", "err", err)
	}
}

func (d *Daemon) handleSessionKill(w http.ResponseWriter, r *http.Request) {
	var req KillSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.ID <= 0 {
		http.Error(w, "id is required and must be positive", http.StatusBadRequest)
		return
	}

	d.mu.Lock()
	var targetKey sessionKey
	var targetManaged *managedSession
	for key, managed := range d.sessions {
		managed.mu.Lock()
		if managed.id == req.ID {
			targetKey = key
			targetManaged = managed
			managed.mu.Unlock()
			break
		}
		managed.mu.Unlock()
	}
	d.mu.Unlock()

	if targetManaged == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	go d.deactivateSessionIfMatch(targetKey, targetManaged, "killed via dashboard")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (d *Daemon) handleDiagnostics(dCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DiagnosticsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if req.File == "" {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}

		timeout := time.Duration(req.Timeout * float64(time.Second))
		resp, err := d.processSingleFile(dCtx, req.File, req.NoWait, timeout)
		if err != nil {
			if errors.Is(err, errUnconfiguredExtension) {
				http.Error(w, err.Error(), http.StatusBadRequest)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Debug("failed to write response", "err", err)
		}
	}
}

func (d *Daemon) handleBatchDiagnostics(dCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req BatchDiagnosticsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if len(req.Files) == 0 {
			http.Error(w, "files is required", http.StatusBadRequest)
			return
		}

		timeout := time.Duration(req.Timeout * float64(time.Second))

		type indexedResult struct {
			index int
			resp  DiagnosticsResponse
		}

		var resultsMu sync.Mutex
		var results []indexedResult

		sem := make(chan struct{}, 10)
		var wg sync.WaitGroup

		for i, file := range req.Files {
			wg.Add(1)
			go func(idx int, filePath string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				resp, err := d.processSingleFile(dCtx, filePath, req.NoWait, timeout)
				if err != nil {
					if !errors.Is(err, errUnconfiguredExtension) {
						slog.Error("batch diagnostics: file failed", "file", filePath, "err", err)
					}
					return
				}

				resultsMu.Lock()
				results = append(results, indexedResult{index: idx, resp: resp})
				resultsMu.Unlock()
			}(i, file)
		}

		wg.Wait()

		sort.Slice(results, func(i, j int) bool {
			return results[i].resp.File < results[j].resp.File
		})

		batchResp := BatchDiagnosticsResponse{
			Files: make([]DiagnosticsResponse, 0, len(results)),
		}
		for _, result := range results {
			batchResp.Files = append(batchResp.Files, result.resp)
			batchResp.TotalErrors += result.resp.ErrorCount
			batchResp.TotalWarnings += result.resp.WarningCount
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(batchResp); err != nil {
			slog.Debug("failed to write response", "err", err)
		}
	}
}

// processSingleFile resolves a file path and returns its diagnostics.
func (d *Daemon) processSingleFile(ctx context.Context, filePath string, noWait bool, timeout time.Duration) (DiagnosticsResponse, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("resolving path: %w", err)
	}
	// Resolve symlinks so our URI matches what the LSP server uses.
	// On macOS, /tmp is a symlink to /private/tmp, and gopls resolves it.
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}

	// Determine workspace root.
	gitRootDir, err := gitRoot(ctx, absPath)
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("finding workspace root: %w", err)
	}

	// Walk up from the file to find the nearest .dowse.toml.
	result, err := config.FindNearest(absPath, gitRootDir, globalConfigPath())
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("loading config: %w", err)
	}

	ext := filepath.Ext(absPath)
	lspCfg, err := result.Config.Resolve(ext)
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("%w: %v", errUnconfiguredExtension, err)
	}

	// Get or create session.
	managed, err := d.getOrCreateSession(ctx, result.ProjectRoot, lspCfg.Command, lspCfg.InitializationOptions, lspCfg.Extensions)
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("session error: %w", err)
	}

	uri := "file://" + absPath

	// Read file content.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return DiagnosticsResponse{}, fmt.Errorf("reading file: %w", err)
	}

	d.ensureFileOpen(ctx, managed, uri, content, ext)

	var diagnostics []cache.Diagnostic
	var stale bool

	if managed.session.DiagModel() == session.DiagnosticPull {
		// Pull model: request diagnostics directly.
		protoDiags, err := managed.session.PullDiagnostics(ctx, uri)
		if err != nil {
			return DiagnosticsResponse{}, fmt.Errorf("pull diagnostics: %w", err)
		}
		diagnostics = convertDiagnostics(protoDiags)
		stale = false
	} else {
		// Push model: wait for fresh diagnostics or return stale.
		if noWait {
			diagnostics, stale = managed.cache.Get(uri)
		} else {
			if timeout <= 0 {
				managed.mu.Lock()
				isFirst := managed.firstRequest
				managed.mu.Unlock()
				if isFirst {
					timeout = firstRequestTimeout
				} else {
					timeout = defaultTimeout
				}
			}
			waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
			defer waitCancel()
			diags, err := managed.cache.WaitForFresh(waitCtx, uri)
			if err != nil {
				// Timeout: return stale diagnostics.
				diagnostics, _ = managed.cache.Get(uri)
				stale = true
			} else {
				diagnostics = diags
				stale = false
			}
			managed.mu.Lock()
			managed.firstRequest = false
			managed.mu.Unlock()
		}
	}

	if diagnostics == nil {
		diagnostics = []cache.Diagnostic{}
	}

	resp := formatDiagnosticsResponse(absPath, result.ProjectRoot, content, diagnostics, stale, lspCfg.MaxSeverity())
	return resp, nil
}

// ensureFileOpen opens or updates a file in the LSP session. The lock is held
// across the LSP notification to prevent concurrent requests from sending
// didChange before didOpen for the same URI.
func (d *Daemon) ensureFileOpen(ctx context.Context, managed *managedSession, uri string, content []byte, ext string) {
	languageID := session.DefaultLanguageID(ext)

	// Grab the per-URI lock to serialize open/change for this file without
	// blocking operations on other URIs.
	managed.mu.Lock()
	managed.lastActivity = time.Now()
	uriLock := managed.fileLock(uri)
	managed.mu.Unlock()

	uriLock.Lock()
	defer uriLock.Unlock()

	managed.mu.Lock()
	version, isOpen := managed.openFiles[uri]
	if !isOpen {
		version = 1
		managed.openFiles[uri] = version
		managed.cache.RecordChange(uri, version)
		managed.mu.Unlock()
		if err := managed.session.OpenFile(ctx, uri, string(content), languageID); err != nil {
			slog.Error("open file failed", "uri", uri, "err", err)
		}
	} else {
		version++
		managed.openFiles[uri] = version
		managed.cache.RecordChange(uri, version)
		managed.mu.Unlock()
		if err := managed.session.ChangeFile(ctx, uri, string(content), version); err != nil {
			slog.Error("change file failed", "uri", uri, "err", err)
		}
	}
}

// ensureFileOpenReadOnly opens a file in the LSP session if it hasn't been
// opened yet, but does nothing if the file is already open. This avoids
// sending unnecessary didChange notifications and invalidating the diagnostics
// cache for read-only operations like go-to-definition.
func (d *Daemon) ensureFileOpenReadOnly(ctx context.Context, managed *managedSession, uri string, content []byte, ext string) {
	managed.mu.Lock()
	_, isOpen := managed.openFiles[uri]
	if isOpen {
		managed.mu.Unlock()
		return
	}
	uriLock := managed.fileLock(uri)
	managed.mu.Unlock()

	uriLock.Lock()
	defer uriLock.Unlock()

	// Re-check under URI lock.
	managed.mu.Lock()
	_, isOpen = managed.openFiles[uri]
	if isOpen {
		managed.mu.Unlock()
		return
	}
	languageID := session.DefaultLanguageID(ext)
	managed.openFiles[uri] = 1
	managed.cache.RecordChange(uri, 1)
	managed.mu.Unlock()
	if err := managed.session.OpenFile(ctx, uri, string(content), languageID); err != nil {
		slog.Error("open file failed", "uri", uri, "err", err)
	}
}

// handleWatcherEvents processes file system events from the watcher and
// forwards them to the LSP session as didOpen/didChange/didClose notifications.
// It runs until the events channel is closed (when the session context is cancelled).
func (d *Daemon) handleWatcherEvents(ctx context.Context, managed *managedSession, events <-chan watcher.Event) {
	for event := range events {
		uri := "file://" + event.Path
		ext := filepath.Ext(event.Path)

		switch event.Kind {
		case watcher.EventCreated, watcher.EventModified:
			content, err := os.ReadFile(event.Path)
			if err != nil {
				slog.Debug("watcher: failed to read file", "path", event.Path, "err", err)
				continue
			}
			d.ensureFileOpen(ctx, managed, uri, content, ext)

		case watcher.EventDeleted:
			managed.mu.Lock()
			uriLock := managed.fileLock(uri)
			managed.mu.Unlock()

			uriLock.Lock()
			managed.mu.Lock()
			_, wasOpen := managed.openFiles[uri]
			if wasOpen {
				delete(managed.openFiles, uri)
				managed.lastActivity = time.Now()
			}
			managed.mu.Unlock()

			if wasOpen {
				if err := managed.session.CloseFile(ctx, uri); err != nil {
					slog.Error("watcher: close file failed", "uri", uri, "err", err)
				}
			}
			uriLock.Unlock()

			if wasOpen {
				managed.cache.Remove(uri)
			}
		}
	}
}

func (d *Daemon) handleDefinition(dCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DefinitionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if req.File == "" {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		if req.Line < 1 || req.Character < 1 {
			http.Error(w, "line and character must be >= 1 (1-indexed)", http.StatusBadRequest)
			return
		}

		resp, err := d.processDefinition(dCtx, req)
		if err != nil {
			if errors.Is(err, errUnconfiguredExtension) {
				http.Error(w, err.Error(), http.StatusBadRequest)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Debug("failed to write response", "err", err)
		}
	}
}

// processDefinition resolves a file path and returns definition locations.
func (d *Daemon) processDefinition(ctx context.Context, req DefinitionRequest) (DefinitionResponse, error) {
	absPath, err := filepath.Abs(req.File)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("resolving path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}

	gitRootDir, err := gitRoot(ctx, absPath)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("finding workspace root: %w", err)
	}

	result, err := config.FindNearest(absPath, gitRootDir, globalConfigPath())
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("loading config: %w", err)
	}

	ext := filepath.Ext(absPath)
	lspCfg, err := result.Config.Resolve(ext)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("%w: %v", errUnconfiguredExtension, err)
	}

	managed, err := d.getOrCreateSession(ctx, result.ProjectRoot, lspCfg.Command, lspCfg.InitializationOptions, lspCfg.Extensions)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("session error: %w", err)
	}

	uri := "file://" + absPath

	content, err := os.ReadFile(absPath)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("reading file: %w", err)
	}

	d.ensureFileOpenReadOnly(ctx, managed, uri, content, ext)

	// Convert 1-indexed (CLI) to 0-indexed (LSP).
	line := uint32(req.Line - 1)
	character := uint32(req.Character - 1)

	locations, err := managed.session.Definition(ctx, uri, line, character)
	if err != nil {
		return DefinitionResponse{}, fmt.Errorf("definition: %w", err)
	}

	return formatDefinitionResponse(locations, result.ProjectRoot), nil
}

func (d *Daemon) getOrCreateSession(ctx context.Context, wsRoot string, lspCmd []string, initOptions map[string]any, extensions []string) (*managedSession, error) {
	key := sessionKey{
		workspaceRoot: wsRoot,
		lspCommand:    strings.Join(lspCmd, "\x00"),
	}

	// Fast path: return existing active session.
	d.mu.Lock()
	if managed, ok := d.sessions[key]; ok {
		managed.mu.Lock()
		isInactive := managed.inactive
		managed.mu.Unlock()
		if !isInactive {
			d.mu.Unlock()
			if err := managed.session.WaitReady(ctx); err != nil {
				return nil, fmt.Errorf("session init failed: %w", err)
			}
			return managed, nil
		}
		// Inactive tombstone — remove it so we recreate below.
		delete(d.sessions, key)
	}
	d.mu.Unlock()

	// Use singleflight to collapse concurrent session creation for the same key.
	singleflightKey := key.workspaceRoot + "\x00" + key.lspCommand
	result, err, _ := d.sessionGroup.Do(singleflightKey, func() (any, error) {
		// Re-check under lock in case another singleflight call just finished.
		d.mu.Lock()
		if managed, ok := d.sessions[key]; ok {
			managed.mu.Lock()
			isInactive := managed.inactive
			managed.mu.Unlock()
			if !isInactive {
				d.mu.Unlock()
				if err := managed.session.WaitReady(ctx); err != nil {
					return nil, fmt.Errorf("session init failed: %w", err)
				}
				return managed, nil
			}
			delete(d.sessions, key)
		}
		d.mu.Unlock()

		// Spawn the LSP process outside the lock.
		lspSession, err := d.factory(ctx, wsRoot, lspCmd, initOptions)
		if err != nil {
			return nil, fmt.Errorf("creating session: %w", err)
		}

		diagCache := cache.New()

		// Wire push diagnostics to cache.
		lspSession.OnDiagnostics(func(uri string, version *int, diagnostics []protocol.Diagnostic) {
			diagCache.Update(uri, version, convertDiagnostics(diagnostics))
		})

		sessionCtx, sessionCancel := context.WithCancel(ctx)

		now := time.Now()
		managed := &managedSession{
			session:      lspSession,
			cache:        diagCache,
			openFiles:    make(map[string]int),
			fileLocks:    make(map[string]*sync.Mutex),
			firstRequest: true,
			createdAt:    now,
			lastActivity: now,
			cancelCtx:    sessionCancel,
			extensions:   extensions,
		}

		// Register the session before initialization so it's visible to
		// status queries and concurrent requests.
		d.mu.Lock()
		d.nextID++
		managed.id = d.nextID
		d.sessions[key] = managed
		d.mu.Unlock()

		// Initialize the LSP handshake. On failure, remove from map.
		if err := managed.session.Initialize(ctx); err != nil {
			sessionCancel()
			d.mu.Lock()
			delete(d.sessions, key)
			d.mu.Unlock()
			return nil, fmt.Errorf("initializing session: %w", err)
		}

		// Monitor for unexpected LSP process exit. Launched after Initialize
		// succeeds to avoid an orphaned goroutine that could deactivate a
		// later session created for the same key.
		go func() {
			select {
			case <-sessionCtx.Done():
				return
			case <-managed.session.Wait():
				d.deactivateSessionIfMatch(key, managed, "LSP process exited unexpectedly")
			}
		}()

		// Start file watcher for this session. Non-fatal on error: the
		// session still works in CLI-driven mode without a watcher.
		fileWatcher, err := watcher.New(wsRoot, extensions)
		if err != nil {
			slog.Error("failed to create file watcher", "workspace", wsRoot, "err", err)
		} else {
			watchEvents, err := fileWatcher.Watch(sessionCtx)
			if err != nil {
				slog.Error("failed to start file watcher", "workspace", wsRoot, "err", err)
			} else {
				go d.handleWatcherEvents(sessionCtx, managed, watchEvents)
			}
		}

		return managed, nil
	})
	if err != nil {
		return nil, err
	}
	managed := result.(*managedSession)

	// Wait for the singleflight winner to finish initialization.
	if err := managed.session.WaitReady(ctx); err != nil {
		return nil, fmt.Errorf("session init failed: %w", err)
	}

	return managed, nil
}

func (d *Daemon) shutdownSessions() {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for key, managed := range d.sessions {
		managed.mu.Lock()
		alreadyInactive := managed.inactive
		managed.inactive = true
		cancelCtx := managed.cancelCtx
		managed.mu.Unlock()
		if alreadyInactive {
			continue
		}
		if cancelCtx != nil {
			cancelCtx()
		}
		if err := managed.session.Shutdown(ctx); err != nil {
			slog.Error("session shutdown failed", "workspace", key.workspaceRoot, "err", err)
		}
	}
	d.sessions = make(map[sessionKey]*managedSession)
}

// deactivateSessionIfMatch is like deactivateSession but only acts if the
// session currently stored at key is the exact same *managedSession that the
// caller holds. This prevents a stale crash monitor from deactivating a
// replacement session that was created at the same key after the original
// was reaped.
func (d *Daemon) deactivateSessionIfMatch(key sessionKey, expected *managedSession, reason string) {
	d.mu.Lock()
	current, exists := d.sessions[key]
	if !exists || current != expected {
		d.mu.Unlock()
		return
	}
	// Mark inactive while still holding d.mu so no replacement can be
	// registered between the pointer check and the state change.
	current.mu.Lock()
	alreadyInactive := current.inactive
	current.inactive = true
	current.mu.Unlock()
	d.mu.Unlock()

	if alreadyInactive {
		return
	}

	slog.Info("deactivating session", "workspace", key.workspaceRoot, "reason", reason)

	// Cancel the per-session context first. This stops the watcher goroutine
	// and crash monitor before we shut down the LSP process.
	if current.cancelCtx != nil {
		current.cancelCtx()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := current.session.Shutdown(ctx); err != nil {
		slog.Error("session shutdown failed during deactivation", "workspace", key.workspaceRoot, "err", err)
	}
}

// startReaper launches a goroutine that periodically deactivates idle sessions.
func (d *Daemon) startReaper(ctx context.Context, idleTTL time.Duration) {
	checkInterval := idleTTL / 2
	if checkInterval < 30*time.Second {
		checkInterval = 30 * time.Second
	}
	ticker := time.NewTicker(checkInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.reapIdleSessions(idleTTL)
			}
		}
	}()
}

// reapIdleSessions finds active sessions idle longer than ttl and deactivates them.
func (d *Daemon) reapIdleSessions(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)

	type reapTarget struct {
		key     sessionKey
		managed *managedSession
	}

	d.mu.Lock()
	var toReap []reapTarget
	for key, managed := range d.sessions {
		managed.mu.Lock()
		idle := !managed.inactive && managed.lastActivity.Before(cutoff)
		managed.mu.Unlock()
		if idle {
			toReap = append(toReap, reapTarget{key: key, managed: managed})
		}
	}
	d.mu.Unlock()

	for _, target := range toReap {
		d.deactivateSessionIfMatch(target.key, target.managed, "idle timeout")
	}
}

func (d *Daemon) loadSessionTTL() time.Duration {
	globalPath := globalConfigPath()
	cfg, err := config.LoadWithPaths(globalPath, "")
	if err != nil {
		return config.DefaultSessionTTL
	}
	return cfg.SessionTTL()
}

// convertDiagnostics maps protocol.Diagnostic to cache.Diagnostic.
func convertDiagnostics(protos []protocol.Diagnostic) []cache.Diagnostic {
	out := make([]cache.Diagnostic, len(protos))
	for i, diag := range protos {
		var severity *int
		if diag.Severity != nil {
			severityInt := int(*diag.Severity)
			severity = &severityInt
		}

		var source string
		if diag.Source != nil {
			source = *diag.Source
		}

		code := extractCode(diag.Code)

		var tags []int
		for _, tag := range diag.Tags {
			tags = append(tags, int(tag))
		}

		out[i] = cache.Diagnostic{
			Range: cache.Range{
				Start: cache.Position{Line: int(diag.Range.Start.Line), Character: int(diag.Range.Start.Character)},
				End:   cache.Position{Line: int(diag.Range.End.Line), Character: int(diag.Range.End.Character)},
			},
			Severity: severity,
			Source:   source,
			Code:     code,
			Tags:     tags,
			Message:  diag.Message,
		}
	}
	return out
}

// extractCode converts the LSP Code field (string or integer) to a string.
func extractCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var codeStr string
	if err := json.Unmarshal(raw, &codeStr); err == nil {
		return codeStr
	}
	var codeInt int
	if err := json.Unmarshal(raw, &codeInt); err == nil {
		return fmt.Sprintf("%d", codeInt)
	}
	return ""
}

// gitRoot finds the git repository root for a file path.
func gitRoot(ctx context.Context, filePath string) (string, error) {
	dir := filepath.Dir(filePath)
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dowseHome returns the base directory for Dowse state files (socket, PID,
// log, global config). It checks the DOWSE_HOME environment variable first,
// falling back to ~/.dowse.
func dowseHome() string {
	if dir := os.Getenv("DOWSE_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("failed to determine home directory, using temp dir", "err", err)
		return filepath.Join(os.TempDir(), ".dowse")
	}
	return filepath.Join(home, ".dowse")
}

// globalConfigPath returns the path to the global Dowse config file.
func globalConfigPath() string {
	return filepath.Join(dowseHome(), "config.toml")
}

// GlobalConfigPath returns the path to the global Dowse config file.
func GlobalConfigPath() string {
	return globalConfigPath()
}

// DefaultSocketPath returns the default daemon socket path.
func DefaultSocketPath() string {
	return filepath.Join(dowseHome(), "dowse.sock")
}

// DefaultPIDPath returns the default daemon PID file path.
func DefaultPIDPath() string {
	return filepath.Join(dowseHome(), "dowse.pid")
}

// DefaultLogPath returns the default daemon log file path.
func DefaultLogPath() string {
	return filepath.Join(dowseHome(), "dowse.log")
}
