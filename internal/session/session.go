// Package session manages individual LSP session lifecycles, including server
// initialization, diagnostic model detection (push vs pull), and file watching.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dreammify/dowse/internal/lsp/protocol"
	"github.com/dreammify/dowse/internal/lsp/transport"
)

// DiagnosticModel indicates whether the LSP server uses push or pull diagnostics.
type DiagnosticModel int

const (
	DiagnosticPush DiagnosticModel = iota
	DiagnosticPull
)

// registeredFileWatcher stores a dynamically registered file watcher pattern.
type registeredFileWatcher struct {
	registrationID string
	globPattern    string // resolved to a simple string pattern
	watchKind      protocol.WatchKind
}

// Session manages a single LSP session for a workspace.
type Session struct {
	proc          *transport.LSPProcess
	workspaceRoot string
	diagModel     DiagnosticModel
	progress      *ProgressTracker
	initOptions   map[string]any

	initDone chan struct{}
	initErr  error

	mu      sync.RWMutex
	handler func(uri string, version *int, diagnostics []protocol.Diagnostic)

	watchersMu sync.RWMutex
	watchers   []registeredFileWatcher
}

// New creates a new LSP session by spawning the given command and wiring
// notification/callback handlers. Call Initialize() to perform the LSP
// handshake. initOptions, if non-nil, will be sent as initializationOptions
// in the initialize request.
func New(ctx context.Context, workspaceRoot string, lspCommand []string, initOptions map[string]any) (*Session, error) {
	s := &Session{
		workspaceRoot: workspaceRoot,
		progress:      NewProgressTracker(),
		initDone:      make(chan struct{}),
		initOptions:   initOptions,
	}

	proc, err := transport.Start(ctx, lspCommand, workspaceRoot, s.handleNotification, s.handleCallback)
	if err != nil {
		return nil, fmt.Errorf("starting LSP process: %w", err)
	}
	s.proc = proc

	return s, nil
}

// Initialize performs the LSP initialize handshake and detects the diagnostic
// model. It must be called exactly once after New().
func (s *Session) Initialize(ctx context.Context) error {
	defer close(s.initDone)

	rootURI := "file://" + s.workspaceRoot
	initParams := protocol.InitializeParams{
		ProcessId: int32(os.Getpid()),
		RootUri:   rootURI,
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				DidChangeWatchedFiles: &protocol.DidChangeWatchedFilesClientCapabilities{
					DynamicRegistration: ptrBool(true),
				},
			},
			TextDocument: &protocol.TextDocumentClientCapabilities{
				PublishDiagnostics: &protocol.PublishDiagnosticsClientCapabilities{},
				Diagnostic:         &protocol.DiagnosticClientCapabilities{},
			},
			Window: &protocol.WindowClientCapabilities{
				WorkDoneProgress: ptrBool(true),
			},
		},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{Uri: rootURI, Name: s.workspaceRoot},
		},
	}

	if s.initOptions != nil {
		raw, err := json.Marshal(s.initOptions)
		if err != nil {
			s.initErr = fmt.Errorf("marshaling initialization options: %w", err)
			return s.initErr
		}
		initParams.InitializationOptions = raw
	}

	var initResult protocol.InitializeResult
	if err := s.proc.Call(ctx, "initialize", initParams, &initResult); err != nil {
		s.initErr = fmt.Errorf("initialize request: %w", err)
		_ = s.proc.Close(ctx)
		return s.initErr
	}

	// Detect diagnostic model from server capabilities.
	diagProvider := initResult.Capabilities.DiagnosticProvider
	if len(diagProvider) > 0 && string(diagProvider) != "null" {
		s.diagModel = DiagnosticPull
	} else {
		s.diagModel = DiagnosticPush
	}

	if err := s.proc.Notify(ctx, "initialized", struct{}{}); err != nil {
		s.initErr = fmt.Errorf("initialized notification: %w", err)
		_ = s.proc.Close(ctx)
		return s.initErr
	}

	return nil
}

// WaitReady blocks until initialization completes and returns any init error.
func (s *Session) WaitReady(ctx context.Context) error {
	select {
	case <-s.initDone:
		return s.initErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Initialized returns true if the LSP handshake has completed.
func (s *Session) Initialized() bool {
	select {
	case <-s.initDone:
		return true
	default:
		return false
	}
}

// handleNotification dispatches incoming LSP server notifications.
func (s *Session) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		s.handlePublishDiagnostics(params)
	case "$/progress":
		var progressParams protocol.ProgressParams
		if err := json.Unmarshal(params, &progressParams); err != nil {
			slog.Warn("failed to unmarshal progress notification", "error", err)
			return
		}
		s.progress.HandleProgress(progressParams)
	case "language/status":
		s.handleLanguageStatus(params)
	}
}

// handleLanguageStatus processes language/status notifications (e.g. from jdtls).
func (s *Session) handleLanguageStatus(params json.RawMessage) {
	var status struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(params, &status); err != nil {
		slog.Warn("failed to unmarshal language/status", "error", err)
		return
	}
	slog.Debug("language/status", "type", status.Type, "message", status.Message)
	s.progress.SetServiceStatus(status.Type, status.Message)
}

func (s *Session) handlePublishDiagnostics(params json.RawMessage) {
	var publishParams protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &publishParams); err != nil {
		slog.Warn("failed to unmarshal publishDiagnostics", "error", err)
		return
	}

	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()

	if handler == nil {
		return
	}

	var version *int
	if publishParams.Version != nil {
		versionInt := int(*publishParams.Version)
		version = &versionInt
	}
	handler(publishParams.Uri, version, publishParams.Diagnostics)
}

// handleCallback responds to server-initiated requests.
func (s *Session) handleCallback(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "window/workDoneProgress/create":
		var createParams protocol.WorkDoneProgressCreateParams
		if err := json.Unmarshal(params, &createParams); err != nil {
			return nil, fmt.Errorf("unmarshal progress create params: %w", err)
		}
		token := normalizeToken(createParams.Token)
		s.progress.RegisterToken(token)
		return nil, nil
	case "client/registerCapability":
		s.handleRegisterCapability(params)
		return nil, nil
	case "client/unregisterCapability":
		s.handleUnregisterCapability(params)
		return nil, nil
	default:
		slog.Warn("unhandled server request", "method", method)
		return nil, nil
	}
}

// handleRegisterCapability processes client/registerCapability requests,
// extracting workspace/didChangeWatchedFiles registrations.
func (s *Session) handleRegisterCapability(params json.RawMessage) {
	var regParams protocol.RegistrationParams
	if err := json.Unmarshal(params, &regParams); err != nil {
		slog.Warn("failed to unmarshal registerCapability", "error", err)
		return
	}

	for _, reg := range regParams.Registrations {
		if reg.Method != "workspace/didChangeWatchedFiles" {
			continue
		}

		if len(reg.RegisterOptions) == 0 {
			continue
		}

		var opts protocol.DidChangeWatchedFilesRegistrationOptions
		if err := json.Unmarshal(reg.RegisterOptions, &opts); err != nil {
			slog.Warn("failed to unmarshal didChangeWatchedFiles options", "error", err, "id", reg.Id)
			continue
		}

		s.watchersMu.Lock()
		for _, fsWatcher := range opts.Watchers {
			globStr := resolveGlobPattern(fsWatcher.GlobPattern)
			if globStr == "" {
				continue
			}
			kind := protocol.WatchKind(protocol.WatchKindCreate | protocol.WatchKindChange | protocol.WatchKindDelete)
			if fsWatcher.Kind != nil {
				kind = *fsWatcher.Kind
			}
			s.watchers = append(s.watchers, registeredFileWatcher{
				registrationID: reg.Id,
				globPattern:    globStr,
				watchKind:      kind,
			})
			slog.Debug("registered file watcher", "id", reg.Id, "pattern", globStr, "kind", kind)
		}
		s.watchersMu.Unlock()
	}
}

// handleUnregisterCapability removes watchers by registration ID.
func (s *Session) handleUnregisterCapability(params json.RawMessage) {
	var unregParams protocol.UnregistrationParams
	if err := json.Unmarshal(params, &unregParams); err != nil {
		slog.Warn("failed to unmarshal unregisterCapability", "error", err)
		return
	}

	s.watchersMu.Lock()
	defer s.watchersMu.Unlock()

	for _, unreg := range unregParams.Unregisterations {
		if unreg.Method != "workspace/didChangeWatchedFiles" {
			continue
		}
		filtered := s.watchers[:0]
		for _, watcher := range s.watchers {
			if watcher.registrationID != unreg.Id {
				filtered = append(filtered, watcher)
			}
		}
		s.watchers = filtered
		slog.Debug("unregistered file watcher", "id", unreg.Id)
	}
}

// resolveGlobPattern extracts a string glob pattern from a GlobPattern
// (which is a union of string | RelativePattern).
func resolveGlobPattern(raw json.RawMessage) string {
	// Try string first.
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}

	// Try RelativePattern.
	var rel protocol.RelativePattern
	if err := json.Unmarshal(raw, &rel); err == nil {
		// BaseUri can be a string (URI) or a WorkspaceFolder.
		var baseURI string
		if err := json.Unmarshal(rel.BaseUri, &baseURI); err != nil {
			// Try WorkspaceFolder.
			var folder struct {
				Uri string `json:"uri"`
			}
			if err := json.Unmarshal(rel.BaseUri, &folder); err == nil {
				baseURI = folder.Uri
			}
		}
		if baseURI != "" {
			basePath := strings.TrimPrefix(baseURI, "file://")
			return filepath.ToSlash(basePath) + "/" + string(rel.Pattern)
		}
		return string(rel.Pattern)
	}

	return ""
}

// NotifyWatchedFileChange sends a workspace/didChangeWatchedFiles notification
// for a file change. Only files matching dynamically registered file watcher
// patterns are forwarded. If no watchers are registered, this is a no-op.
func (s *Session) NotifyWatchedFileChange(ctx context.Context, filePath string, changeType protocol.FileChangeType) {
	s.watchersMu.RLock()
	if len(s.watchers) == 0 {
		s.watchersMu.RUnlock()
		return
	}
	matched := false
	normalizedPath := filepath.ToSlash(filePath)
	for _, watcher := range s.watchers {
		if !watchKindMatches(watcher.watchKind, changeType) {
			continue
		}
		if MatchGlob(watcher.globPattern, normalizedPath) || MatchGlob(watcher.globPattern, filepath.Base(normalizedPath)) {
			matched = true
			break
		}
	}
	s.watchersMu.RUnlock()

	if !matched {
		return
	}

	uri := "file://" + filePath
	params := protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{Uri: uri, Type: changeType},
		},
	}
	if err := s.proc.Notify(ctx, "workspace/didChangeWatchedFiles", params); err != nil {
		slog.Error("failed to send didChangeWatchedFiles", "file", filePath, "error", err)
	}
	slog.Debug("sent didChangeWatchedFiles", "file", filePath, "type", changeType)
}

// watchKindMatches checks if a FileChangeType matches a WatchKind bitmask.
func watchKindMatches(kind protocol.WatchKind, changeType protocol.FileChangeType) bool {
	switch changeType {
	case protocol.FileChangeTypeCreated:
		return kind&protocol.WatchKindCreate != 0
	case protocol.FileChangeTypeChanged:
		return kind&protocol.WatchKindChange != 0
	case protocol.FileChangeTypeDeleted:
		return kind&protocol.WatchKindDelete != 0
	default:
		return false
	}
}

// Progress returns a human-readable summary of active progress, or empty string if idle.
func (s *Session) Progress() string {
	status, _ := s.ProgressWithPercent()
	return status
}

// ProgressWithPercent returns the progress status and percentage separately.
// Returns ("initializing", nil) if the LSP handshake hasn't completed yet
// and there is no active progress from the tracker.
func (s *Session) ProgressWithPercent() (string, *uint32) {
	status, percent := s.progress.StatusWithPercent()
	if status == "" && !s.Initialized() {
		return "initializing", nil
	}
	return status, percent
}

// OpenFile notifies the LSP that a file is open with the given content.
func (s *Session) OpenFile(ctx context.Context, uri string, content string, languageID protocol.LanguageKind) error {
	params := protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			Uri:        uri,
			LanguageId: languageID,
			Version:    1,
			Text:       content,
		},
	}
	return s.proc.Notify(ctx, "textDocument/didOpen", params)
}

// ChangeFile notifies the LSP that an open file's content has changed.
func (s *Session) ChangeFile(ctx context.Context, uri string, content string, version int) error {
	changeEvent, err := json.Marshal(protocol.TextDocumentContentChangeWholeDocument{
		Text: content,
	})
	if err != nil {
		return fmt.Errorf("marshaling change event: %w", err)
	}

	params := protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			Uri:     uri,
			Version: int32(version),
		},
		ContentChanges: []json.RawMessage{changeEvent},
	}
	return s.proc.Notify(ctx, "textDocument/didChange", params)
}

// CloseFile notifies the LSP that a file is no longer open.
func (s *Session) CloseFile(ctx context.Context, uri string) error {
	params := protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{
			Uri: uri,
		},
	}
	return s.proc.Notify(ctx, "textDocument/didClose", params)
}

// PullDiagnostics sends a textDocument/diagnostic request and returns the result.
func (s *Session) PullDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	params := protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{
			Uri: uri,
		},
	}

	var report protocol.RelatedFullDocumentDiagnosticReport
	if err := s.proc.Call(ctx, "textDocument/diagnostic", params, &report); err != nil {
		return nil, fmt.Errorf("textDocument/diagnostic: %w", err)
	}
	return report.Items, nil
}

// Definition sends a textDocument/definition request and returns the result
// as a normalized []Location (empty for null/no result).
func (s *Session) Definition(ctx context.Context, uri string, line uint32, character uint32) ([]protocol.Location, error) {
	params := protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{Uri: uri},
		Position:     protocol.Position{Line: line, Character: character},
	}

	var rawResult json.RawMessage
	if err := s.proc.Call(ctx, "textDocument/definition", params, &rawResult); err != nil {
		return nil, fmt.Errorf("textDocument/definition: %w", err)
	}
	return parseDefinitionResult(rawResult)
}

// parseDefinitionResult normalizes the LSP definition response (Location |
// Location[] | null) into a []Location. Returns an empty slice for null.
func parseDefinitionResult(raw json.RawMessage) ([]protocol.Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []protocol.Location{}, nil
	}

	// Try array first.
	var locations []protocol.Location
	if err := json.Unmarshal(raw, &locations); err == nil {
		return locations, nil
	}

	// Try single Location.
	var single protocol.Location
	if err := json.Unmarshal(raw, &single); err == nil {
		return []protocol.Location{single}, nil
	}

	return nil, fmt.Errorf("unexpected definition result: %s", string(raw))
}

// PID returns the process ID of the underlying LSP process.
func (s *Session) PID() int {
	return s.proc.PID()
}

// DiagModel returns whether this LSP uses push or pull diagnostics.
func (s *Session) DiagModel() DiagnosticModel {
	return s.diagModel
}

// OnDiagnostics registers a callback for push-model publishDiagnostics notifications.
func (s *Session) OnDiagnostics(handler func(uri string, version *int, diagnostics []protocol.Diagnostic)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

// Shutdown gracefully shuts down the LSP process.
func (s *Session) Shutdown(ctx context.Context) error {
	return s.proc.Close(ctx)
}

// Wait returns a channel that is closed when the LSP process exits.
func (s *Session) Wait() <-chan struct{} {
	return s.proc.Wait()
}

// DefaultLanguageID returns the LSP language identifier for a file extension.
// Returns an empty string for unrecognized extensions.
func DefaultLanguageID(ext string) protocol.LanguageKind {
	switch ext {
	case ".go":
		return protocol.LanguageKindGo
	case ".java":
		return protocol.LanguageKindJava
	case ".kt", ".kts":
		return "kotlin"
	case ".ts":
		return protocol.LanguageKindTypeScript
	case ".tsx":
		return protocol.LanguageKindTypeScriptReact
	case ".js":
		return protocol.LanguageKindJavaScript
	case ".jsx":
		return protocol.LanguageKindJavaScriptReact
	case ".py":
		return protocol.LanguageKindPython
	case ".rb", ".rbi":
		return protocol.LanguageKindRuby
	case ".rs":
		return protocol.LanguageKindRust
	default:
		return ""
	}
}

func ptrBool(v bool) *bool { return &v }
