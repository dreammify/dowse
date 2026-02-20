package main

import (
	"fmt"
	"strings"

	"github.com/dreammify/dowse/internal/daemon"
)

// severityIcon returns a symbol for each diagnostic severity level.
func severityIcon(severity string) string {
	switch severity {
	case "error":
		return "✘"
	case "warning":
		return "⚠"
	case "information":
		return "ℹ"
	case "hint":
		return "•"
	default:
		return "✘"
	}
}

// formatDiagnosticsText renders a single-file DiagnosticsResponse as
// human-readable text with severity icons and inline code context.
func formatDiagnosticsText(resp daemon.DiagnosticsResponse) string {
	if len(resp.Diagnostics) == 0 {
		return ""
	}

	var builder strings.Builder
	for _, diag := range resp.Diagnostics {
		icon := severityIcon(diag.Severity)
		fmt.Fprintf(&builder, "\n  %s [Line %d] %s\n", icon, diag.Line, diag.Message)
		for _, line := range diag.Context {
			fmt.Fprintf(&builder, "    %s\n", line)
		}
	}
	builder.WriteString("\n")
	return builder.String()
}

// formatBatchDiagnosticsText renders a BatchDiagnosticsResponse as
// human-readable text, grouping diagnostics by file.
func formatBatchDiagnosticsText(resp daemon.BatchDiagnosticsResponse) string {
	var builder strings.Builder

	for _, fileResp := range resp.Files {
		if len(fileResp.Diagnostics) == 0 {
			continue
		}
		fmt.Fprintf(&builder, "%s\n", fileResp.File)
		builder.WriteString(formatDiagnosticsText(fileResp))
	}

	if resp.TotalErrors > 0 || resp.TotalWarnings > 0 {
		var parts []string
		if resp.TotalErrors > 0 {
			parts = append(parts, fmt.Sprintf("%d error(s)", resp.TotalErrors))
		}
		if resp.TotalWarnings > 0 {
			parts = append(parts, fmt.Sprintf("%d warning(s)", resp.TotalWarnings))
		}
		fmt.Fprintf(&builder, "Found %s.\n", strings.Join(parts, ", "))
	}

	return builder.String()
}
