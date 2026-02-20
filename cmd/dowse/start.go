package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/dreammify/dowse/internal/session"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	var foreground bool

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the dowse daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			socketPath := daemon.DefaultSocketPath()

			// Check if daemon is already running.
			if pingDaemon(socketPath) {
				fmt.Fprintln(os.Stderr, "daemon is already running")
				return nil
			}

			if foreground {
				return runDaemonForeground(socketPath)
			}
			return startDaemonBackground(socketPath)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run daemon in the current process")
	return cmd
}

func runDaemonForeground(socketPath string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	d := daemon.New(func(ctx context.Context, workspaceRoot string, lspCmd []string, initOptions map[string]any) (daemon.LSPSession, error) {
		return session.New(ctx, workspaceRoot, lspCmd, initOptions)
	})
	return d.Run(ctx, socketPath)
}

func startDaemonBackground(socketPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	logPath := daemon.DefaultLogPath()
	pidPath := daemon.DefaultPIDPath()

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}

	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = daemonSysProcAttr()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("starting daemon process: %w", err)
	}

	// Write PID file.
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		logFile.Close()
		return fmt.Errorf("writing PID file: %w", err)
	}

	logFile.Close()

	// Wait for socket to appear and daemon to respond to ping.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pingDaemon(socketPath) {
			fmt.Fprintf(os.Stderr, "daemon started (pid %d)\n", cmd.Process.Pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("daemon did not start within 5 seconds")
}

func pingDaemon(socketPath string) bool {
	client := daemon.NewUnixClient(socketPath, 2*time.Second)
	resp, err := client.Get("http://dowse/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
