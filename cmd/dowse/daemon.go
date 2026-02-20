package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/dreammify/dowse/internal/session"
	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the daemon directly (internal use)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			socketPath := daemon.DefaultSocketPath()
			pidPath := daemon.DefaultPIDPath()

			// Write PID file (the start command also does this, but we write
			// it here too for the case where daemon is run directly).
			if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
				return fmt.Errorf("creating dowse directory: %w", err)
			}
			if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				return fmt.Errorf("writing PID file: %w", err)
			}
			defer os.Remove(pidPath)
			defer os.Remove(socketPath)

			d := daemon.New(func(ctx context.Context, workspaceRoot string, lspCmd []string, initOptions map[string]any) (daemon.LSPSession, error) {
				return session.New(ctx, workspaceRoot, lspCmd, initOptions)
			})
			return d.Run(ctx, socketPath)
		},
	}
	return cmd
}
