package transport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dreammify/dowse/internal/lsp/transport"
)

func TestProcessLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proc, err := transport.Start(ctx, []string{"cat"}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Process should be running.
	select {
	case <-proc.Wait():
		t.Fatal("process exited prematurely")
	default:
	}

	// Kill the process (cat won't respond to shutdown).
	// We close with a short context since cat won't handle JSON-RPC.
	closeCtx, closeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer closeCancel()
	_ = proc.Close(closeCtx) // expected to fail since cat doesn't speak JSON-RPC

	// Process should exit.
	select {
	case <-proc.Wait():
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after Close")
	}
}

func TestCallResponseRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use bash as a mock server that reads requests and writes responses.
	proc, err := transport.Start(ctx, []string{
		"bash", "-c",
		// Read one LSP-framed request, extract the id, return a response.
		`while IFS= read -r line; do
			if [[ "$line" =~ ^Content-Length:\ ([0-9]+) ]]; then
				len="${BASH_REMATCH[1]}"
				read -r blank  # empty line after headers
				body=$(head -c "$len")
				id=$(echo "$body" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('id',''))")
				method=$(echo "$body" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('method',''))")
				if [ "$method" = "shutdown" ]; then
					response='{"jsonrpc":"2.0","result":null,"id":'$id'}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				elif [ "$method" = "exit" ]; then
					exit 0
				elif [ "$method" = "test/echo" ]; then
					params=$(echo "$body" | python3 -c "import sys,json; print(json.dumps(json.loads(sys.stdin.read()).get('params',{})))")
					response='{"jsonrpc":"2.0","result":'"$params"',"id":'$id'}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				fi
			fi
		done`,
	}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		proc.Close(closeCtx)
	}()

	// Test Call with echo.
	type echoParams struct {
		Message string `json:"message"`
	}
	var result echoParams
	err = proc.Call(ctx, "test/echo", &echoParams{Message: "hello"}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Message != "hello" {
		t.Errorf("got message %q, want %q", result.Message, "hello")
	}
}

func TestNotificationReceiving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	notifyCh := make(chan string, 10)

	// Mock server that sends a notification after receiving a request.
	proc, err := transport.Start(ctx, []string{
		"bash", "-c",
		`while IFS= read -r line; do
			if [[ "$line" =~ ^Content-Length:\ ([0-9]+) ]]; then
				len="${BASH_REMATCH[1]}"
				read -r blank
				body=$(head -c "$len")
				id=$(echo "$body" | python3 -c "import sys,json; d=json.loads(sys.stdin.read()); print(d.get('id','null'))" 2>/dev/null)
				method=$(echo "$body" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('method',''))" 2>/dev/null)
				if [ "$method" = "shutdown" ]; then
					response='{"jsonrpc":"2.0","result":null,"id":'$id'}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				elif [ "$method" = "exit" ]; then
					exit 0
				elif [ "$method" = "test/triggerNotify" ]; then
					# First send a notification
					notif='{"jsonrpc":"2.0","method":"test/notification","params":{"data":"from-server"}}'
					printf "Content-Length: %d\r\n\r\n%s" "${#notif}" "$notif"
					# Then send the response
					response='{"jsonrpc":"2.0","result":{"ok":true},"id":'$id'}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				fi
			fi
		done`,
	}, "", func(method string, params json.RawMessage) {
		notifyCh <- method
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		proc.Close(closeCtx)
	}()

	// Trigger a notification from the server.
	var result struct {
		OK bool `json:"ok"`
	}
	err = proc.Call(ctx, "test/triggerNotify", nil, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !result.OK {
		t.Error("expected ok=true")
	}

	// Wait for the notification.
	select {
	case method := <-notifyCh:
		if method != "test/notification" {
			t.Errorf("got notification method %q, want %q", method, "test/notification")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestProcessCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start a process that exits immediately.
	proc, err := transport.Start(ctx, []string{"bash", "-c", "exit 0"}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the process to exit.
	select {
	case <-proc.Wait():
		// OK - process exited.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for process exit")
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start a process that never responds.
	proc, err := transport.Start(ctx, []string{"sleep", "60"}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Try to call with a short timeout.
	callCtx, callCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer callCancel()

	var result json.RawMessage
	err = proc.Call(callCtx, "test/slow", nil, &result)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	// Clean up.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer closeCancel()
	_ = proc.Close(closeCtx)
}

// TestNonStandardResponseFields verifies that responses containing non-standard
// JSON-RPC fields (like Sorbet's "requestMethod") are handled without error.
func TestNonStandardResponseFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Mock server that includes a non-standard "requestMethod" field in
	// responses, matching Sorbet's actual behavior.
	proc, err := transport.Start(ctx, []string{
		"bash", "-c",
		`while IFS= read -r line; do
			if [[ "$line" =~ ^Content-Length:\ ([0-9]+) ]]; then
				len="${BASH_REMATCH[1]}"
				read -r blank
				body=$(head -c "$len")
				id=$(echo "$body" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('id',''))")
				method=$(echo "$body" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('method',''))")
				if [ "$method" = "shutdown" ]; then
					response='{"jsonrpc":"2.0","result":null,"id":'$id',"requestMethod":"shutdown"}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				elif [ "$method" = "exit" ]; then
					exit 0
				elif [ "$method" = "test/echo" ]; then
					params=$(echo "$body" | python3 -c "import sys,json; print(json.dumps(json.loads(sys.stdin.read()).get('params',{})))")
					response='{"jsonrpc":"2.0","result":'"$params"',"id":'$id',"requestMethod":"test/echo"}'
					printf "Content-Length: %d\r\n\r\n%s" "${#response}" "$response"
				fi
			fi
		done`,
	}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		proc.Close(closeCtx)
	}()

	type echoParams struct {
		Message string `json:"message"`
	}
	var result echoParams
	err = proc.Call(ctx, "test/echo", &echoParams{Message: "hello"}, &result)
	if err != nil {
		t.Fatalf("Call failed (non-standard fields should be tolerated): %v", err)
	}
	if result.Message != "hello" {
		t.Errorf("got message %q, want %q", result.Message, "hello")
	}
}

func TestNotifySending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start a process that reads notifications.
	// Use cat so it just consumes input.
	proc, err := transport.Start(ctx, []string{"cat"}, "", nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send a notification - should not block.
	err = proc.Notify(ctx, "test/notification", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// Clean up.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer closeCancel()
	_ = proc.Close(closeCtx)
}
