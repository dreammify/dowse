package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "dowse",
		Short: "LSP bridge for AI agents",
	}
	root.AddCommand(
		newStartCmd(),
		newStopCmd(),
		newDiagnosticsCmd(),
		newDefinitionCmd(),
		newStatusCmd(),
		newDaemonCmd(),
		newMcpCmd(),
		newDashboardCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
