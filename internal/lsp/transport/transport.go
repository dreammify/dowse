// Package transport provides a JSON-RPC stdio transport for LSP servers,
// wrapping jrpc2 with child process management and Content-Length framing.
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
)

// NotificationHandler is a function that handles an incoming server notification.
type NotificationHandler func(method string, params json.RawMessage)

// CallbackHandler handles server-initiated requests (e.g., window/workDoneProgress/create).
// It receives the method and raw JSON params, and returns a result to send back.
type CallbackHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// LSPProcess manages a child LSP process and its JSON-RPC connection.
type LSPProcess struct {
	cmd    *exec.Cmd
	client *jrpc2.Client

	mu   sync.Mutex
	done chan struct{} // closed when the process exits
	err  error         // process exit error
}

// Start spawns the LSP command and establishes the JSON-RPC connection.
// The onNotification callback, if non-nil, is called for each server notification.
// The onCallback callback, if non-nil, is called for each server-initiated request.
func Start(ctx context.Context, command []string, onNotification NotificationHandler, onCallback CallbackHandler) (*LSPProcess, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting command %v: %w", command, err)
	}

	ch := lenientLSP(stdout, stdin)

	opts := &jrpc2.ClientOptions{}
	if onNotification != nil {
		opts.OnNotify = func(req *jrpc2.Request) {
			onNotification(req.Method(), []byte(req.ParamString()))
		}
	}
	if onCallback != nil {
		opts.OnCallback = func(ctx context.Context, req *jrpc2.Request) (any, error) {
			return onCallback(ctx, req.Method(), []byte(req.ParamString()))
		}
	}

	client := jrpc2.NewClient(ch, opts)

	p := &LSPProcess{
		cmd:    cmd,
		client: client,
		done:   make(chan struct{}),
	}

	// Monitor process exit in background.
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()

	return p, nil
}

// Call sends a JSON-RPC request and decodes the response into result.
func (p *LSPProcess) Call(ctx context.Context, method string, params any, result any) error {
	return p.client.CallResult(ctx, method, params, result)
}

// Notify sends a JSON-RPC notification (no response expected).
func (p *LSPProcess) Notify(ctx context.Context, method string, params any) error {
	return p.client.Notify(ctx, method, params)
}

// Wait returns a channel that closes when the LSP process exits.
func (p *LSPProcess) Wait() <-chan struct{} {
	return p.done
}

// Close sends shutdown/exit to the LSP and kills the process if needed.
func (p *LSPProcess) Close(ctx context.Context) error {
	// Send shutdown request.
	_, err := p.client.Call(ctx, "shutdown", nil)
	if err != nil {
		// If shutdown fails, try to kill the process.
		_ = p.cmd.Process.Kill()
		_ = p.client.Close()
		<-p.done
		return fmt.Errorf("shutdown request failed: %w", err)
	}

	// Send exit notification.
	_ = p.client.Notify(ctx, "exit", nil)

	// Close the client channel.
	_ = p.client.Close()

	// Wait for the process to exit.
	<-p.done
	return nil
}

// PID returns the process ID of the LSP child process.
func (p *LSPProcess) PID() int {
	return p.cmd.Process.Pid
}

// ExitError returns the process exit error, or nil if still running.
func (p *LSPProcess) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// lenientLSP creates a channel using standard LSP framing that tolerates
// any Content-Type from the server. Some LSP servers (e.g. JetBrains
// Kotlin LSP) send "application/json-rpc" instead of the standard
// "application/vscode-jsonrpc", which the strict channel.LSP rejects.
func lenientLSP(r io.Reader, wc io.WriteCloser) channel.Channel {
	return &lenientChannel{inner: channel.LSP(r, wc)}
}

type lenientChannel struct {
	inner channel.Channel
}

func (c *lenientChannel) Send(msg []byte) error { return c.inner.Send(msg) }
func (c *lenientChannel) Close() error          { return c.inner.Close() }

func (c *lenientChannel) Recv() ([]byte, error) {
	msg, err := c.inner.Recv()
	if _, ok := err.(*channel.ContentTypeMismatchError); ok {
		err = nil
	}
	return msg, err
}
