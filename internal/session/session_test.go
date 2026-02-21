package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/dreammify/dowse/internal/lsp/protocol"
	"github.com/dreammify/dowse/internal/session"
)

// pushMockServer returns a python3 command that acts as a push-model LSP server.
// It responds to initialize without diagnosticProvider in capabilities.
// When it receives didOpen, it sends a publishDiagnostics notification for that file.
func pushMockServer() []string {
	return []string{
		"python3", "-u", "-c", `
import sys, json

def read_msg():
    headers = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        line = line.decode('utf-8').strip()
        if line == '':
            break
        if ':' in line:
            k, v = line.split(':', 1)
            headers[k.strip().lower()] = v.strip()
    length = int(headers.get('content-length', 0))
    if length == 0:
        return None
    body = sys.stdin.buffer.read(length)
    return json.loads(body)

def send_msg(obj):
    body = json.dumps(obj).encode('utf-8')
    header = ("Content-Length: %d\r\n\r\n" % len(body)).encode('utf-8')
    sys.stdout.buffer.write(header + body)
    sys.stdout.buffer.flush()

while True:
    msg = read_msg()
    if msg is None:
        break
    method = msg.get('method', '')
    mid = msg.get('id')

    if method == 'initialize':
        send_msg({"jsonrpc":"2.0","result":{"capabilities":{"textDocumentSync":1}},"id":mid})
    elif method == 'initialized':
        pass
    elif method == 'textDocument/didOpen':
        uri = msg['params']['textDocument']['uri']
        send_msg({"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{
            "uri": uri, "version": 1,
            "diagnostics": [{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}},"severity":1,"message":"test error"}]
        }})
    elif method in ('textDocument/didChange', 'textDocument/didClose'):
        pass
    elif method == 'shutdown':
        send_msg({"jsonrpc":"2.0","result":None,"id":mid})
    elif method == 'exit':
        break
`,
	}
}

// pullMockServer returns a python3 command that acts as a pull-model LSP server.
// It responds to initialize with diagnosticProvider in capabilities and
// responds to textDocument/diagnostic requests.
func pullMockServer() []string {
	return []string{
		"python3", "-u", "-c", `
import sys, json

def read_msg():
    headers = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        line = line.decode('utf-8').strip()
        if line == '':
            break
        if ':' in line:
            k, v = line.split(':', 1)
            headers[k.strip().lower()] = v.strip()
    length = int(headers.get('content-length', 0))
    if length == 0:
        return None
    body = sys.stdin.buffer.read(length)
    return json.loads(body)

def send_msg(obj):
    body = json.dumps(obj).encode('utf-8')
    header = ("Content-Length: %d\r\n\r\n" % len(body)).encode('utf-8')
    sys.stdout.buffer.write(header + body)
    sys.stdout.buffer.flush()

while True:
    msg = read_msg()
    if msg is None:
        break
    method = msg.get('method', '')
    mid = msg.get('id')

    if method == 'initialize':
        send_msg({"jsonrpc":"2.0","result":{"capabilities":{"textDocumentSync":1,"diagnosticProvider":{"interFileDependencies":True,"workspaceDiagnostics":False}}},"id":mid})
    elif method == 'initialized':
        pass
    elif method in ('textDocument/didOpen', 'textDocument/didChange', 'textDocument/didClose'):
        pass
    elif method == 'textDocument/diagnostic':
        send_msg({"jsonrpc":"2.0","result":{"kind":"full","items":[{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":10}},"severity":2,"message":"unused variable"}]},"id":mid})
    elif method == 'shutdown':
        send_msg({"jsonrpc":"2.0","result":None,"id":mid})
    elif method == 'exit':
        break
`,
	}
}

func TestInitializePushModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pushMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if s.DiagModel() != session.DiagnosticPush {
		t.Errorf("got DiagModel=%d, want DiagnosticPush(%d)", s.DiagModel(), session.DiagnosticPush)
	}
}

func TestInitializePullModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pullMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if s.DiagModel() != session.DiagnosticPull {
		t.Errorf("got DiagModel=%d, want DiagnosticPull(%d)", s.DiagModel(), session.DiagnosticPull)
	}
}

func TestDidOpenChangeClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pushMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	uri := "file://" + workspaceDir + "/main.go"

	if err := s.OpenFile(ctx, uri, "package main\n", "go"); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	if err := s.ChangeFile(ctx, uri, "package main\n\nfunc main() {}\n", 2); err != nil {
		t.Fatalf("ChangeFile: %v", err)
	}

	if err := s.CloseFile(ctx, uri); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}
}

func TestPushDiagnosticsNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pushMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	type diagEvent struct {
		URI         string
		Version     *int
		Diagnostics []protocol.Diagnostic
	}
	ch := make(chan diagEvent, 1)

	s.OnDiagnostics(func(uri string, version *int, diagnostics []protocol.Diagnostic) {
		ch <- diagEvent{URI: uri, Version: version, Diagnostics: diagnostics}
	})

	// didOpen triggers the mock to send a publishDiagnostics notification.
	uri := "file://" + workspaceDir + "/main.go"
	if err := s.OpenFile(ctx, uri, "package main\n", "go"); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.URI != uri {
			t.Errorf("got URI %q, want %q", ev.URI, uri)
		}
		if ev.Version == nil {
			t.Error("expected non-nil version")
		} else if *ev.Version != 1 {
			t.Errorf("got version %d, want 1", *ev.Version)
		}
		if len(ev.Diagnostics) != 1 {
			t.Fatalf("got %d diagnostics, want 1", len(ev.Diagnostics))
		}
		if ev.Diagnostics[0].Message != "test error" {
			t.Errorf("got message %q, want %q", ev.Diagnostics[0].Message, "test error")
		}
		if ev.Diagnostics[0].Severity == nil || *ev.Diagnostics[0].Severity != protocol.DiagnosticSeverityError {
			t.Errorf("got severity %v, want Error(%d)", ev.Diagnostics[0].Severity, protocol.DiagnosticSeverityError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for publishDiagnostics notification")
	}
}

func TestPullDiagnostics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pullMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	diags, err := s.PullDiagnostics(ctx, "file://"+workspaceDir+"/main.go")
	if err != nil {
		t.Fatalf("PullDiagnostics: %v", err)
	}

	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(diags))
	}
	if diags[0].Message != "unused variable" {
		t.Errorf("got message %q, want %q", diags[0].Message, "unused variable")
	}
	if diags[0].Severity == nil || *diags[0].Severity != protocol.DiagnosticSeverityWarning {
		t.Errorf("got severity %v, want Warning(%d)", diags[0].Severity, protocol.DiagnosticSeverityWarning)
	}
	if diags[0].Range.Start.Line != 1 || diags[0].Range.Start.Character != 0 {
		t.Errorf("got range start (%d,%d), want (1,0)", diags[0].Range.Start.Line, diags[0].Range.Start.Character)
	}
	if diags[0].Range.End.Line != 1 || diags[0].Range.End.Character != 10 {
		t.Errorf("got range end (%d,%d), want (1,10)", diags[0].Range.End.Line, diags[0].Range.End.Character)
	}
}

// callbackMockServer is a mock LSP server that sends client/registerCapability
// during initialization to verify the session handles it without error.
func callbackMockServer() []string {
	return []string{
		"python3", "-u", "-c", `
import sys, json

def read_msg():
    headers = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        line = line.decode('utf-8').strip()
        if line == '':
            break
        if ':' in line:
            k, v = line.split(':', 1)
            headers[k.strip().lower()] = v.strip()
    length = int(headers.get('content-length', 0))
    if length == 0:
        return None
    body = sys.stdin.buffer.read(length)
    return json.loads(body)

def send_msg(obj):
    body = json.dumps(obj).encode('utf-8')
    header = ("Content-Length: %d\r\n\r\n" % len(body)).encode('utf-8')
    sys.stdout.buffer.write(header + body)
    sys.stdout.buffer.flush()

req_id = 100

while True:
    msg = read_msg()
    if msg is None:
        break
    method = msg.get('method', '')
    mid = msg.get('id')

    if method == 'initialize':
        send_msg({"jsonrpc":"2.0","result":{"capabilities":{"textDocumentSync":1}},"id":mid})
    elif method == 'initialized':
        # Send client/registerCapability request to the client.
        send_msg({"jsonrpc":"2.0","id":req_id,"method":"client/registerCapability","params":{"registrations":[{"id":"1","method":"workspace/didChangeWatchedFiles"}]}})
        req_id += 1
    elif method == 'shutdown':
        send_msg({"jsonrpc":"2.0","result":None,"id":mid})
    elif method == 'exit':
        break
`,
	}
}

func TestCallbackRegisterCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, callbackMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = s.Shutdown(shutCtx)
	}()

	// Initialize should succeed even though the server sends client/registerCapability.
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestDefaultLanguageID(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".go", "go"},
		{".java", "java"},
		{".kt", "kotlin"},
		{".kts", "kotlin"},
		{".ts", "typescript"},
		{".tsx", "typescriptreact"},
		{".js", "javascript"},
		{".jsx", "javascriptreact"},
		{".py", "python"},
		{".rb", "ruby"},
		{".rbi", "ruby"},
		{".rs", "rust"},
		{".unknown", ""},
	}
	for _, tt := range tests {
		got := string(session.DefaultLanguageID(tt.ext))
		if got != tt.want {
			t.Errorf("DefaultLanguageID(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}

func TestShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceDir := t.TempDir()
	s, err := session.New(ctx, workspaceDir, pushMockServer(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	shutCtx, shutCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shutCancel()

	if err := s.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-s.Wait():
		// OK - process exited.
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after Shutdown")
	}
}
