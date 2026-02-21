// Package mcpserver implements an MCP stdio server that exposes Dowse's
// diagnostics and definition capabilities as tools for AI hosts.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
	"github.com/creachadair/jrpc2/handler"
	"github.com/dreammify/dowse/internal/daemon"
)

// MCP protocol types.

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ClientInfo      Implementation `json:"clientInfo"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	ServerInfo      Implementation   `json:"serverInfo"`
	Capabilities    ServerCapability `json:"capabilities"`
}

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerCapability struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct{}

type ToolsListResult struct {
	Tools []ToolDef `json:"tools"`
}

type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]PropertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

type PropertySchema struct {
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Items       *PropertySchema `json:"items,omitempty"`
}

type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolsCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DaemonClient abstracts communication with the dowse daemon.
type DaemonClient interface {
	GetDiagnostics(ctx context.Context, file string) ([]byte, error)
	GetBatchDiagnostics(ctx context.Context, files []string) ([]byte, error)
	GetDefinition(ctx context.Context, file string, line, character int) ([]byte, error)
}

// HTTPDaemonClient talks to the daemon over its Unix socket HTTP API.
type HTTPDaemonClient struct {
	client *http.Client
}

// NewHTTPDaemonClient creates a client that connects to the daemon at socketPath.
func NewHTTPDaemonClient(socketPath string) *HTTPDaemonClient {
	return &HTTPDaemonClient{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", socketPath, time.Second)
				},
			},
			Timeout: 65 * time.Second,
		},
	}
}

func (h *HTTPDaemonClient) GetDiagnostics(ctx context.Context, file string) ([]byte, error) {
	reqBody, err := json.Marshal(daemon.DiagnosticsRequest{File: file})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	return h.post(ctx, "/diagnostics", reqBody)
}

func (h *HTTPDaemonClient) GetBatchDiagnostics(ctx context.Context, files []string) ([]byte, error) {
	reqBody, err := json.Marshal(daemon.BatchDiagnosticsRequest{Files: files})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	return h.post(ctx, "/diagnostics/batch", reqBody)
}

func (h *HTTPDaemonClient) GetDefinition(ctx context.Context, file string, line, character int) ([]byte, error) {
	reqBody, err := json.Marshal(daemon.DefinitionRequest{File: file, Line: line, Character: character})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	return h.post(ctx, "/definition", reqBody)
}

func (h *HTTPDaemonClient) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://dowse"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon error (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return bytes.TrimRight(respBody, "\n"), nil
}

// Server is an MCP server that bridges tool calls to the dowse daemon.
type Server struct {
	mux          handler.Map
	client       DaemonClient
	allowedTools []string // nil = all tools exposed; non-nil = only listed tools
}

// New creates an MCP server backed by the given DaemonClient.
// allowedTools controls which tools are exposed: nil means all tools,
// a non-nil slice restricts to only the named tools.
func New(client DaemonClient, allowedTools []string) *Server {
	server := &Server{client: client, allowedTools: allowedTools}
	server.mux = handler.Map{
		"initialize":                handler.New(server.handleInitialize),
		"notifications/initialized": handler.New(server.handleInitialized),
		"tools/list":                handler.New(server.handleToolsList),
		"tools/call":                handler.New(server.handleToolsCall),
	}
	return server
}

// Serve starts the MCP server on stdio using newline-delimited JSON-RPC.
// It blocks until the client disconnects or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeChannel(ctx, channel.Line(os.Stdin, os.Stdout))
}

// ServeChannel starts the MCP server on the given channel.
// It blocks until the client disconnects or ctx is cancelled.
func (s *Server) ServeChannel(ctx context.Context, ch channel.Channel) error {
	srv := jrpc2.NewServer(s.mux, nil)
	srv.Start(ch)

	// Stop the server if the context is cancelled.
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()

	return srv.Wait()
}

func (s *Server) handleInitialize(_ context.Context, params InitializeParams) (InitializeResult, error) {
	return InitializeResult{
		ProtocolVersion: "2025-03-26",
		ServerInfo:      Implementation{Name: "dowse", Version: "0.1.0"},
		Capabilities:    ServerCapability{Tools: &ToolsCapability{}},
	}, nil
}

func (s *Server) handleInitialized(_ context.Context) error {
	return nil
}

func (s *Server) handleToolsList(_ context.Context) (ToolsListResult, error) {
	if s.allowedTools == nil {
		return ToolsListResult{Tools: allToolDefs}, nil
	}
	allowed := make(map[string]bool, len(s.allowedTools))
	for _, name := range s.allowedTools {
		allowed[name] = true
	}
	var filtered []ToolDef
	for _, tool := range allToolDefs {
		if allowed[tool.Name] {
			filtered = append(filtered, tool)
		}
	}
	return ToolsListResult{Tools: filtered}, nil
}

func (s *Server) isToolAllowed(name string) bool {
	for _, allowed := range s.allowedTools {
		if allowed == name {
			return true
		}
	}
	return false
}

func (s *Server) handleToolsCall(ctx context.Context, params ToolsCallParams) (ToolsCallResult, error) {
	if s.allowedTools != nil && !s.isToolAllowed(params.Name) {
		return toolError(fmt.Sprintf("tool not enabled: %s", params.Name)), nil
	}
	switch params.Name {
	case "get_diagnostics":
		return s.callGetDiagnostics(ctx, params.Arguments)
	case "get_definition":
		return s.callGetDefinition(ctx, params.Arguments)
	default:
		return toolError(fmt.Sprintf("unknown tool: %s", params.Name)), nil
	}
}

func (s *Server) callGetDiagnostics(ctx context.Context, rawArgs json.RawMessage) (ToolsCallResult, error) {
	var args struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Files) == 0 {
		return toolError("missing required parameter: files"), nil
	}

	result, err := s.client.GetBatchDiagnostics(ctx, args.Files)
	if err != nil {
		return toolError(fmt.Sprintf("daemon error: %v", err)), nil
	}

	return ToolsCallResult{
		Content: []ContentBlock{{Type: "text", Text: string(result)}},
	}, nil
}

func (s *Server) callGetDefinition(ctx context.Context, rawArgs json.RawMessage) (ToolsCallResult, error) {
	var args struct {
		File      string `json:"file"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.File == "" {
		return toolError("missing required parameter: file"), nil
	}
	if args.Line < 1 {
		return toolError("missing required parameter: line"), nil
	}
	if args.Character < 1 {
		return toolError("missing required parameter: character"), nil
	}

	result, err := s.client.GetDefinition(ctx, args.File, args.Line, args.Character)
	if err != nil {
		return toolError(fmt.Sprintf("daemon error: %v", err)), nil
	}

	return ToolsCallResult{
		Content: []ContentBlock{{Type: "text", Text: string(result)}},
	}, nil
}

func toolError(message string) ToolsCallResult {
	return ToolsCallResult{
		Content: []ContentBlock{{Type: "text", Text: message}},
		IsError: true,
	}
}
