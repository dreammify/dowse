package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/dreammify/dowse/internal/mcpserver"
	"github.com/spf13/cobra"
)

func newMcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run as an MCP stdio server",
		Long:  "Starts an MCP server on stdin/stdout. Designed to be launched by an AI host (e.g., claude mcp add -s user -t stdio dowse -- dowse mcp).",
		RunE: func(cmd *cobra.Command, args []string) error {
			// All logging goes to stderr; stdout is reserved for MCP JSON-RPC.
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

			socketPath := daemon.DefaultSocketPath()

			// Auto-start daemon if not running.
			if !pingDaemon(socketPath) {
				slog.Info("daemon not running, starting it")
				if err := startDaemonBackground(socketPath); err != nil {
					return fmt.Errorf("auto-starting daemon: %w", err)
				}
			}

			client := mcpserver.NewHTTPDaemonClient(socketPath)
			server := mcpserver.New(client)
			return server.Serve(cmd.Context())
		},
	}
}
