package main

import (
	"fmt"
	"os"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the dowse daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			socketPath := daemon.DefaultSocketPath()
			pidPath := daemon.DefaultPIDPath()

			client := daemon.NewUnixClient(socketPath, 5*time.Second)

			resp, err := client.Post("http://dowse/shutdown", "", nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, "daemon is not running")
				return nil
			}
			resp.Body.Close()

			// Wait for socket to disappear.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !pingDaemon(socketPath) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}

			// Clean up PID file.
			os.Remove(pidPath)

			fmt.Fprintln(os.Stderr, "daemon stopped")
			return nil
		},
	}
}
