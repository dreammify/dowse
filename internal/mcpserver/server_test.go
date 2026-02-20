package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
)

// mockDaemonClient is a test double for DaemonClient.
type mockDaemonClient struct {
	diagnosticsResult      []byte
	diagnosticsErr         error
	batchDiagnosticsResult []byte
	batchDiagnosticsErr    error
	definitionResult       []byte
	definitionErr          error

	lastFile         string
	lastFiles        []string
	lastDefFile      string
	lastDefLine      int
	lastDefCharacter int
}

func (m *mockDaemonClient) GetDiagnostics(_ context.Context, file string) ([]byte, error) {
	m.lastFile = file
	return m.diagnosticsResult, m.diagnosticsErr
}

func (m *mockDaemonClient) GetBatchDiagnostics(_ context.Context, files []string) ([]byte, error) {
	m.lastFiles = files
	return m.batchDiagnosticsResult, m.batchDiagnosticsErr
}

func (m *mockDaemonClient) GetDefinition(_ context.Context, file string, line, character int) ([]byte, error) {
	m.lastDefFile = file
	m.lastDefLine = line
	m.lastDefCharacter = character
	return m.definitionResult, m.definitionErr
}

// --- Direct handler tests (unit-level) ---

// callMethod invokes a handler method on the server directly via the handler.Map.
func callMethod(t *testing.T, server *Server, method string, params any) (json.RawMessage, error) {
	t.Helper()

	handler := server.mux.Assign(context.Background(), method)
	if handler == nil {
		return nil, fmt.Errorf("no handler for method %q", method)
	}

	var paramBytes []byte
	if params != nil {
		var err error
		paramBytes, err = json.Marshal(params)
		if err != nil {
			t.Fatalf("marshaling params: %v", err)
		}
	}

	req, err := jrpc2.ParseRequests(marshalJRPC(t, "1", method, paramBytes))
	if err != nil {
		t.Fatalf("parsing request: %v", err)
	}

	result, err := handler(context.Background(), req[0].ToRequest())
	if err != nil {
		return nil, err
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshaling result: %v", err)
	}
	return resultBytes, nil
}

// marshalJRPC builds a raw JSON-RPC request message.
func marshalJRPC(t *testing.T, id, method string, params []byte) []byte {
	t.Helper()
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = json.RawMessage(params)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling JRPC message: %v", err)
	}
	return data
}

func TestInitialize(t *testing.T) {
	server := New(&mockDaemonClient{})

	result, err := callMethod(t, server, "initialize", InitializeParams{
		ProtocolVersion: "2025-03-26",
		ClientInfo:      Implementation{Name: "test-client", Version: "1.0"},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	var initResult InitializeResult
	if err := json.Unmarshal(result, &initResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if initResult.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol version = %q, want %q", initResult.ProtocolVersion, "2025-03-26")
	}
	if initResult.ServerInfo.Name != "dowse" {
		t.Errorf("server name = %q, want %q", initResult.ServerInfo.Name, "dowse")
	}
	if initResult.ServerInfo.Version != "0.1.0" {
		t.Errorf("server version = %q, want %q", initResult.ServerInfo.Version, "0.1.0")
	}
	if initResult.Capabilities.Tools == nil {
		t.Error("tools capability is nil, want non-nil")
	}
}

func TestToolsList(t *testing.T) {
	server := New(&mockDaemonClient{})

	result, err := callMethod(t, server, "tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	var listResult ToolsListResult
	if err := json.Unmarshal(result, &listResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if len(listResult.Tools) != 3 {
		t.Fatalf("tool count = %d, want 3", len(listResult.Tools))
	}

	toolsByName := map[string]ToolDef{}
	for _, tool := range listResult.Tools {
		toolsByName[tool.Name] = tool
	}

	// Verify get_diagnostics tool schema.
	diagTool, ok := toolsByName["get_diagnostics"]
	if !ok {
		t.Fatal("missing tool get_diagnostics")
	}
	if diagTool.InputSchema.Type != "object" {
		t.Errorf("get_diagnostics schema type = %q, want %q", diagTool.InputSchema.Type, "object")
	}
	if _, hasFile := diagTool.InputSchema.Properties["file"]; !hasFile {
		t.Error("get_diagnostics missing 'file' property")
	}
	if len(diagTool.InputSchema.Required) != 1 || diagTool.InputSchema.Required[0] != "file" {
		t.Errorf("get_diagnostics required = %v, want [file]", diagTool.InputSchema.Required)
	}
	if diagTool.Description == "" {
		t.Error("get_diagnostics has empty description")
	}

	// Verify get_diagnostics_batch tool schema.
	batchTool, ok := toolsByName["get_diagnostics_batch"]
	if !ok {
		t.Fatal("missing tool get_diagnostics_batch")
	}
	filesProp, hasFiles := batchTool.InputSchema.Properties["files"]
	if !hasFiles {
		t.Fatal("get_diagnostics_batch missing 'files' property")
	}
	if filesProp.Type != "array" {
		t.Errorf("files property type = %q, want %q", filesProp.Type, "array")
	}
	if filesProp.Items == nil {
		t.Fatal("files property missing items schema")
	}
	if filesProp.Items.Type != "string" {
		t.Errorf("files items type = %q, want %q", filesProp.Items.Type, "string")
	}
	if len(batchTool.InputSchema.Required) != 1 || batchTool.InputSchema.Required[0] != "files" {
		t.Errorf("get_diagnostics_batch required = %v, want [files]", batchTool.InputSchema.Required)
	}

	// Verify get_definition tool schema.
	defTool, ok := toolsByName["get_definition"]
	if !ok {
		t.Fatal("missing tool get_definition")
	}
	if defTool.InputSchema.Type != "object" {
		t.Errorf("get_definition schema type = %q, want %q", defTool.InputSchema.Type, "object")
	}
	if _, hasFile := defTool.InputSchema.Properties["file"]; !hasFile {
		t.Error("get_definition missing 'file' property")
	}
	if _, hasLine := defTool.InputSchema.Properties["line"]; !hasLine {
		t.Error("get_definition missing 'line' property")
	}
	if _, hasChar := defTool.InputSchema.Properties["character"]; !hasChar {
		t.Error("get_definition missing 'character' property")
	}
	if len(defTool.InputSchema.Required) != 3 {
		t.Errorf("get_definition required = %v, want [file line character]", defTool.InputSchema.Required)
	}
}

func TestToolsCallGetDiagnostics(t *testing.T) {
	mock := &mockDaemonClient{
		diagnosticsResult: []byte(`{"file":"/tmp/main.go","diagnostics":[],"error_count":0}`),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go"}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if callResult.IsError {
		t.Error("expected isError=false, got true")
	}
	if len(callResult.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(callResult.Content))
	}
	if callResult.Content[0].Type != "text" {
		t.Errorf("content type = %q, want %q", callResult.Content[0].Type, "text")
	}
	if callResult.Content[0].Text != string(mock.diagnosticsResult) {
		t.Errorf("content text = %q, want %q", callResult.Content[0].Text, string(mock.diagnosticsResult))
	}
	if mock.lastFile != "/tmp/main.go" {
		t.Errorf("daemon called with file = %q, want %q", mock.lastFile, "/tmp/main.go")
	}
}

func TestToolsCallGetDiagnosticsBatch(t *testing.T) {
	mock := &mockDaemonClient{
		batchDiagnosticsResult: []byte(`{"files":[],"total_errors":0,"total_warnings":0}`),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics_batch",
		Arguments: json.RawMessage(`{"files":["/tmp/a.go","/tmp/b.go"]}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if callResult.IsError {
		t.Error("expected isError=false, got true")
	}
	if len(callResult.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(callResult.Content))
	}
	if callResult.Content[0].Text != string(mock.batchDiagnosticsResult) {
		t.Errorf("content text mismatch")
	}
	if len(mock.lastFiles) != 2 || mock.lastFiles[0] != "/tmp/a.go" || mock.lastFiles[1] != "/tmp/b.go" {
		t.Errorf("daemon called with files = %v, want [/tmp/a.go /tmp/b.go]", mock.lastFiles)
	}
}

func TestToolsCallMissingFile(t *testing.T) {
	server := New(&mockDaemonClient{})

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if callResult.Content[0].Text != "missing required parameter: file" {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestToolsCallMissingFiles(t *testing.T) {
	server := New(&mockDaemonClient{})

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics_batch",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if callResult.Content[0].Text != "missing required parameter: files" {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestToolsCallDaemonError(t *testing.T) {
	mock := &mockDaemonClient{
		diagnosticsErr: fmt.Errorf("connection refused"),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go"}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if callResult.Content[0].Text != "daemon error: connection refused" {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestToolsCallBatchDaemonError(t *testing.T) {
	mock := &mockDaemonClient{
		batchDiagnosticsErr: fmt.Errorf("timeout exceeded"),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics_batch",
		Arguments: json.RawMessage(`{"files":["/tmp/a.go"]}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if !strings.Contains(callResult.Content[0].Text, "timeout exceeded") {
		t.Errorf("error text = %q, want substring %q", callResult.Content[0].Text, "timeout exceeded")
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	server := New(&mockDaemonClient{})

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "nonexistent",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if callResult.Content[0].Text != "unknown tool: nonexistent" {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestE2EInvalidJSONArguments(t *testing.T) {
	client := startTestServer(t, &mockDaemonClient{})
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{"file": 12345}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call should not return JSON-RPC error: %v", err)
	}

	// file is present but wrong type — json.Unmarshal succeeds, but the value
	// becomes "" (zero value for string), triggering missing-parameter error.
	if !callResult.IsError {
		t.Error("expected isError=true for wrong-type argument")
	}
}

func TestIsErrorOmittedWhenFalse(t *testing.T) {
	// Verify that isError is omitted from the JSON when false (omitempty).
	result := ToolsCallResult{
		Content: []ContentBlock{{Type: "text", Text: "ok"}},
		IsError: false,
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	if strings.Contains(string(data), "isError") {
		t.Errorf("isError should be omitted when false, got: %s", string(data))
	}

	// And present when true.
	result.IsError = true
	data, err = json.Marshal(result)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	if !strings.Contains(string(data), `"isError":true`) {
		t.Errorf("isError should be present when true, got: %s", string(data))
	}
}

// --- End-to-end tests through jrpc2 server + channel ---

// startTestServer creates an MCP server and a jrpc2.Client connected via in-memory channels.
func startTestServer(t *testing.T, mock *mockDaemonClient) *jrpc2.Client {
	t.Helper()
	server := New(mock)
	clientCh, serverCh := channel.Direct()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = server.ServeChannel(ctx, serverCh)
	}()

	client := jrpc2.NewClient(clientCh, nil)
	t.Cleanup(func() { client.Close() })
	return client
}

func TestE2EInitializeAndToolsList(t *testing.T) {
	client := startTestServer(t, &mockDaemonClient{})
	ctx := context.Background()

	// Initialize.
	var initResult InitializeResult
	err := client.CallResult(ctx, "initialize", InitializeParams{
		ProtocolVersion: "2025-03-26",
		ClientInfo:      Implementation{Name: "test", Version: "1.0"},
	}, &initResult)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if initResult.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol version = %q, want %q", initResult.ProtocolVersion, "2025-03-26")
	}
	if initResult.Capabilities.Tools == nil {
		t.Error("tools capability is nil")
	}

	// Send notifications/initialized (should not error).
	if err := client.Notify(ctx, "notifications/initialized", nil); err != nil {
		t.Fatalf("notifications/initialized: %v", err)
	}

	// List tools.
	var listResult ToolsListResult
	err = client.CallResult(ctx, "tools/list", nil, &listResult)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listResult.Tools) != 3 {
		t.Errorf("tool count = %d, want 3", len(listResult.Tools))
	}
}

func TestE2EGetDiagnosticsHappyPath(t *testing.T) {
	diagJSON := `{"file":"/tmp/main.go","diagnostics":[{"message":"unused variable"}],"error_count":1,"warning_count":0}`
	mock := &mockDaemonClient{
		diagnosticsResult: []byte(diagJSON),
	}
	client := startTestServer(t, mock)
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go"}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	if callResult.IsError {
		t.Errorf("expected success, got error: %s", callResult.Content[0].Text)
	}
	if len(callResult.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(callResult.Content))
	}
	if callResult.Content[0].Text != diagJSON {
		t.Errorf("content text = %q, want %q", callResult.Content[0].Text, diagJSON)
	}
	if mock.lastFile != "/tmp/main.go" {
		t.Errorf("daemon file = %q, want %q", mock.lastFile, "/tmp/main.go")
	}
}

func TestE2EGetDiagnosticsBatchHappyPath(t *testing.T) {
	batchJSON := `{"files":[{"file":"/tmp/a.go","diagnostics":[],"error_count":0,"warning_count":0}],"total_errors":0,"total_warnings":0}`
	mock := &mockDaemonClient{
		batchDiagnosticsResult: []byte(batchJSON),
	}
	client := startTestServer(t, mock)
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics_batch",
		Arguments: json.RawMessage(`{"files":["/tmp/a.go"]}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	if callResult.IsError {
		t.Errorf("expected success, got error: %s", callResult.Content[0].Text)
	}
	if callResult.Content[0].Text != batchJSON {
		t.Errorf("content text mismatch")
	}
	if len(mock.lastFiles) != 1 || mock.lastFiles[0] != "/tmp/a.go" {
		t.Errorf("daemon files = %v", mock.lastFiles)
	}
}

func TestE2EDaemonErrorReturnsMCPToolError(t *testing.T) {
	mock := &mockDaemonClient{
		diagnosticsErr: fmt.Errorf("session init failed"),
	}
	client := startTestServer(t, mock)
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go"}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call should not return JSON-RPC error: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true")
	}
	if !strings.Contains(callResult.Content[0].Text, "session init failed") {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestE2EMissingParamReturnsMCPToolError(t *testing.T) {
	client := startTestServer(t, &mockDaemonClient{})
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_diagnostics",
		Arguments: json.RawMessage(`{}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call should not return JSON-RPC error: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true")
	}
	if callResult.Content[0].Text != "missing required parameter: file" {
		t.Errorf("error text = %q", callResult.Content[0].Text)
	}
}

func TestE2EUnknownMethodReturnsJRPCError(t *testing.T) {
	client := startTestServer(t, &mockDaemonClient{})
	ctx := context.Background()

	// Calling an unregistered method should return a JSON-RPC level error.
	var result json.RawMessage
	err := client.CallResult(ctx, "unknown/method", nil, &result)
	if err == nil {
		t.Fatal("expected JSON-RPC error for unknown method, got nil")
	}
}

func TestE2EMultipleSequentialCalls(t *testing.T) {
	mock := &mockDaemonClient{
		diagnosticsResult:      []byte(`{"file":"a","diagnostics":[]}`),
		batchDiagnosticsResult: []byte(`{"files":[],"total_errors":0,"total_warnings":0}`),
	}
	client := startTestServer(t, mock)
	ctx := context.Background()

	// Call get_diagnostics, then batch, then get_diagnostics again.
	for i, params := range []ToolsCallParams{
		{Name: "get_diagnostics", Arguments: json.RawMessage(`{"file":"/tmp/first.go"}`)},
		{Name: "get_diagnostics_batch", Arguments: json.RawMessage(`{"files":["/tmp/batch.go"]}`)},
		{Name: "get_diagnostics", Arguments: json.RawMessage(`{"file":"/tmp/second.go"}`)},
	} {
		var callResult ToolsCallResult
		err := client.CallResult(ctx, "tools/call", params, &callResult)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if callResult.IsError {
			t.Errorf("call %d: unexpected error: %s", i, callResult.Content[0].Text)
		}
	}

	// Verify the last single-file call got the right argument.
	if mock.lastFile != "/tmp/second.go" {
		t.Errorf("last file = %q, want %q", mock.lastFile, "/tmp/second.go")
	}
}

// --- HTTPDaemonClient tests against a real HTTP server ---

func TestHTTPDaemonClientGetDiagnostics(t *testing.T) {
	expectedResp := `{"file":"/tmp/test.go","diagnostics":[],"error_count":0}`
	socketPath, cleanup := startFakeHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/diagnostics" {
			t.Errorf("path = %q, want /diagnostics", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if req["file"] != "/tmp/test.go" {
			t.Errorf("request file = %v, want /tmp/test.go", req["file"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, expectedResp)
	})
	defer cleanup()

	client := NewHTTPDaemonClient(socketPath)
	result, err := client.GetDiagnostics(context.Background(), "/tmp/test.go")
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	if string(result) != expectedResp {
		t.Errorf("result = %q, want %q", string(result), expectedResp)
	}
}

func TestHTTPDaemonClientGetBatchDiagnostics(t *testing.T) {
	expectedResp := `{"files":[],"total_errors":0,"total_warnings":0}`
	socketPath, cleanup := startFakeHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/diagnostics/batch" {
			t.Errorf("path = %q, want /diagnostics/batch", r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		files, ok := req["files"].([]any)
		if !ok || len(files) != 2 {
			t.Errorf("request files = %v", req["files"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, expectedResp)
	})
	defer cleanup()

	client := NewHTTPDaemonClient(socketPath)
	result, err := client.GetBatchDiagnostics(context.Background(), []string{"/tmp/a.go", "/tmp/b.go"})
	if err != nil {
		t.Fatalf("GetBatchDiagnostics: %v", err)
	}
	if string(result) != expectedResp {
		t.Errorf("result = %q, want %q", string(result), expectedResp)
	}
}

func TestHTTPDaemonClientNon200Error(t *testing.T) {
	socketPath, cleanup := startFakeHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "file not found", http.StatusBadRequest)
	})
	defer cleanup()

	client := NewHTTPDaemonClient(socketPath)
	_, err := client.GetDiagnostics(context.Background(), "/tmp/missing.go")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "daemon error (400)") {
		t.Errorf("error = %q, want substring %q", err.Error(), "daemon error (400)")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("error = %q, want substring %q", err.Error(), "file not found")
	}
}

// Regression: json.Encoder (used by the daemon) appends \n after each JSON
// object. The HTTP client must strip it so the MCP tool response text doesn't
// contain a trailing newline that would break channel.Line framing.
func TestHTTPDaemonClientTrimsTrailingNewline(t *testing.T) {
	expectedJSON := `{"file":"/tmp/test.go","diagnostics":[],"error_count":0}`
	socketPath, cleanup := startFakeHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simulate json.Encoder behavior: JSON followed by \n.
		fmt.Fprintln(w, expectedJSON)
	})
	defer cleanup()

	client := NewHTTPDaemonClient(socketPath)
	result, err := client.GetDiagnostics(context.Background(), "/tmp/test.go")
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	if string(result) != expectedJSON {
		t.Errorf("result = %q, want %q (trailing newline not trimmed)", string(result), expectedJSON)
	}
}

// Regression: error response bodies from http.Error also include a trailing \n.
// The error message must be trimmed so it's clean in the error string.
func TestHTTPDaemonClientTrimsErrorBodyNewline(t *testing.T) {
	socketPath, cleanup := startFakeHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		// http.Error appends \n to the body.
		http.Error(w, "unconfigured extension", http.StatusBadRequest)
	})
	defer cleanup()

	client := NewHTTPDaemonClient(socketPath)
	_, err := client.GetDiagnostics(context.Background(), "/tmp/test.xyz")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error message should not end with a newline.
	if strings.HasSuffix(err.Error(), "\n") {
		t.Errorf("error message has trailing newline: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unconfigured extension") {
		t.Errorf("error = %q, want substring %q", err.Error(), "unconfigured extension")
	}
}

func TestHTTPDaemonClientConnectionRefused(t *testing.T) {
	// Use a socket path that doesn't exist.
	client := NewHTTPDaemonClient("/tmp/nonexistent-dowse-test.sock")
	_, err := client.GetDiagnostics(context.Background(), "/tmp/test.go")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
	if !strings.Contains(err.Error(), "connecting to daemon") {
		t.Errorf("error = %q, want substring %q", err.Error(), "connecting to daemon")
	}
}

func TestToolsCallGetDefinition(t *testing.T) {
	defJSON := `{"locations":[{"file":"main.go","line":10,"character":5,"context":"func hello()"}]}`
	mock := &mockDaemonClient{
		definitionResult: []byte(defJSON),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_definition",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go","line":10,"character":5}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if callResult.IsError {
		t.Error("expected isError=false, got true")
	}
	if len(callResult.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(callResult.Content))
	}
	if callResult.Content[0].Text != defJSON {
		t.Errorf("content text = %q, want %q", callResult.Content[0].Text, defJSON)
	}
	if mock.lastDefFile != "/tmp/main.go" {
		t.Errorf("daemon called with file = %q, want %q", mock.lastDefFile, "/tmp/main.go")
	}
	if mock.lastDefLine != 10 {
		t.Errorf("daemon called with line = %d, want 10", mock.lastDefLine)
	}
	if mock.lastDefCharacter != 5 {
		t.Errorf("daemon called with character = %d, want 5", mock.lastDefCharacter)
	}
}

func TestToolsCallGetDefinitionMissingParams(t *testing.T) {
	server := New(&mockDaemonClient{})

	tests := []struct {
		name     string
		args     string
		wantText string
	}{
		{"missing file", `{"line":1,"character":1}`, "missing required parameter: file"},
		{"missing line", `{"file":"/tmp/a.go","character":1}`, "missing required parameter: line"},
		{"missing character", `{"file":"/tmp/a.go","line":1}`, "missing required parameter: character"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := callMethod(t, server, "tools/call", ToolsCallParams{
				Name:      "get_definition",
				Arguments: json.RawMessage(testCase.args),
			})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}

			var callResult ToolsCallResult
			if err := json.Unmarshal(result, &callResult); err != nil {
				t.Fatalf("unmarshaling result: %v", err)
			}

			if !callResult.IsError {
				t.Error("expected isError=true, got false")
			}
			if callResult.Content[0].Text != testCase.wantText {
				t.Errorf("error text = %q, want %q", callResult.Content[0].Text, testCase.wantText)
			}
		})
	}
}

func TestToolsCallGetDefinitionDaemonError(t *testing.T) {
	mock := &mockDaemonClient{
		definitionErr: fmt.Errorf("session not ready"),
	}
	server := New(mock)

	result, err := callMethod(t, server, "tools/call", ToolsCallParams{
		Name:      "get_definition",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go","line":1,"character":1}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var callResult ToolsCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	if !callResult.IsError {
		t.Error("expected isError=true, got false")
	}
	if !strings.Contains(callResult.Content[0].Text, "session not ready") {
		t.Errorf("error text = %q, want substring %q", callResult.Content[0].Text, "session not ready")
	}
}

func TestE2EGetDefinitionHappyPath(t *testing.T) {
	defJSON := `{"locations":[{"file":"main.go","line":5,"character":1,"context":"func main()"}]}`
	mock := &mockDaemonClient{
		definitionResult: []byte(defJSON),
	}
	client := startTestServer(t, mock)
	ctx := context.Background()

	var callResult ToolsCallResult
	err := client.CallResult(ctx, "tools/call", ToolsCallParams{
		Name:      "get_definition",
		Arguments: json.RawMessage(`{"file":"/tmp/main.go","line":5,"character":1}`),
	}, &callResult)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	if callResult.IsError {
		t.Errorf("expected success, got error: %s", callResult.Content[0].Text)
	}
	if callResult.Content[0].Text != defJSON {
		t.Errorf("content text = %q, want %q", callResult.Content[0].Text, defJSON)
	}
}

// startFakeHTTPDaemon starts an HTTP server on a temporary Unix socket.
// Uses /tmp directly because macOS has a 104-byte limit on Unix socket paths
// and t.TempDir() paths are too long.
func startFakeHTTPDaemon(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("/tmp", "dowse-test-*.sock")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	socketPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(socketPath) // net.Listen needs the path to not exist

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listening on %s: %v", socketPath, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	return socketPath, func() {
		srv.Close()
		listener.Close()
		os.Remove(socketPath)
	}
}
