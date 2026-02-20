package daemon

import (
	"context"
	"net"
	"net/http"
	"time"
)

// NewUnixClient creates an HTTP client that connects to the daemon over a Unix
// domain socket. The timeout controls the overall HTTP request timeout.
func NewUnixClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, time.Second)
			},
		},
		Timeout: timeout,
	}
}
