package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dreammify/dowse/internal/daemon"
	"github.com/spf13/cobra"
)

func newDefinitionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "definition <file> <line> <character>",
		Short: "Go to definition at a position in a file",
		Long:  "Returns the definition location(s) for the symbol at the given position. Line and character are 1-indexed.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolving path: %w", err)
			}

			line, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid line number: %w", err)
			}

			character, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("invalid character number: %w", err)
			}

			socketPath := daemon.DefaultSocketPath()
			client := daemon.NewUnixClient(socketPath, 30*time.Second)

			reqBody, err := json.Marshal(daemon.DefinitionRequest{
				File:      filePath,
				Line:      line,
				Character: character,
			})
			if err != nil {
				return fmt.Errorf("marshaling request: %w", err)
			}

			resp, err := client.Post("http://dowse/definition", "application/json", bytes.NewReader(reqBody))
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

			var buf bytes.Buffer
			if err := json.Indent(&buf, body, "", "  "); err != nil {
				return fmt.Errorf("formatting JSON: %w", err)
			}
			fmt.Fprintln(os.Stdout, buf.String())
			return nil
		},
	}
}
