package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/dreammify/dowse/internal/tui"
	"github.com/spf13/cobra"
)

func newDashboardCmd() *cobra.Command {
	var pollInterval time.Duration

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open a live TUI dashboard for monitoring LSP sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			socketPath := daemon.DefaultSocketPath()

			// Auto-start daemon if not running.
			if !pingDaemon(socketPath) {
				fmt.Fprintln(os.Stderr, "daemon not running, starting...")
				if err := startDaemonBackground(socketPath); err != nil {
					return fmt.Errorf("starting daemon: %w", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			return tui.Run(ctx, socketPath, pollInterval)
		},
	}
	cmd.Flags().DurationVar(&pollInterval, "poll", 2*time.Second, "polling interval for dashboard updates")
	return cmd
}
