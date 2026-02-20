package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of all active LSP sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			socketPath := daemon.DefaultSocketPath()

			client := daemon.NewUnixClient(socketPath, 5*time.Second)

			resp, err := client.Get("http://dowse/status")
			if err != nil {
				return fmt.Errorf("connecting to daemon: %w (is the daemon running?)", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("reading response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon error: %s", string(body))
			}

			var statuses []daemon.SessionStatus
			if err := json.Unmarshal(body, &statuses); err != nil {
				return fmt.Errorf("parsing response: %w", err)
			}

			if len(statuses) == 0 {
				fmt.Println("no active sessions")
				return nil
			}

			// Print table header.
			fmt.Printf("%-40s %-20s %-15s %-10s %s\n", "WORKSPACE", "LSP", "STATUS", "IDLE", "FILES")
			for _, session := range statuses {
				statusStr := session.Status
				if session.Percent != nil {
					statusStr = fmt.Sprintf("%s (%d%%)", session.Status, *session.Percent)
				}
				fmt.Printf("%-40s %-20s %-15s %-10s %d\n",
					session.Workspace, session.LSP, statusStr, session.IdleFor, session.OpenFiles)
			}
			return nil
		},
	}
}
