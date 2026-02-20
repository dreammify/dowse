package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
)

// Client talks to the dowse daemon over its Unix socket HTTP API.
type Client struct {
	httpClient *http.Client
}

// NewClient creates a client that connects to the daemon at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					dialer := net.Dialer{Timeout: 3 * time.Second}
				return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 3 * time.Second,
		},
	}
}

// FetchDashboard retrieves the current dashboard state from the daemon.
func (c *Client) FetchDashboard(ctx context.Context) (*daemon.DashboardResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dowse/dashboard", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching dashboard: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("dashboard returned %d: %s", resp.StatusCode, body)
	}

	var dashboard daemon.DashboardResponse
	if err := json.NewDecoder(resp.Body).Decode(&dashboard); err != nil {
		return nil, fmt.Errorf("decoding dashboard: %w", err)
	}
	return &dashboard, nil
}

// KillSession requests the daemon to kill a session by ID.
func (c *Client) KillSession(ctx context.Context, id int) error {
	body, err := json.Marshal(daemon.KillSessionRequest{ID: id})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://dowse/session/kill", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kill request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kill returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// Ping checks if the daemon is reachable.
func (c *Client) Ping(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dowse/ping", nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Shutdown requests the daemon to shut down.
func (c *Client) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://dowse/shutdown", nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	resp.Body.Close()
	return nil
}
