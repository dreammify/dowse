package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dreammify/dowse/internal/cache"
	"github.com/dreammify/dowse/internal/lsp/protocol"
)

// EnrichedDiagnostic is the per-diagnostic response type with human-readable
// fields and inline code context.
type EnrichedDiagnostic struct {
	Line     int      `json:"line"`
	EndLine  int      `json:"end_line,omitempty"`
	Severity string   `json:"severity"`
	Source   string   `json:"source,omitempty"`
	Code     string   `json:"code,omitempty"`
	Message  string   `json:"message"`
	Context  []string `json:"context"`
}

const contextMessageMaxLen = 60

// severityString converts an LSP severity integer to a human-readable string.
// Nil defaults to "error".
func severityString(severity *int) string {
	if severity == nil {
		return "error"
	}
	switch protocol.DiagnosticSeverity(*severity) {
	case protocol.DiagnosticSeverityError:
		return "error"
	case protocol.DiagnosticSeverityWarning:
		return "warning"
	case protocol.DiagnosticSeverityInformation:
		return "information"
	case protocol.DiagnosticSeverityHint:
		return "hint"
	default:
		return "error"
	}
}

// severityValue returns the LSP severity integer for a diagnostic.
// Nil defaults to 1 (error).
func severityValue(severity *int) int {
	if severity == nil {
		return 1
	}
	return *severity
}

// formatDiagnosticsResponse transforms raw cache diagnostics into an enriched
// response with relative paths, 1-indexed lines, string severities, and inline
// code context with pointer annotations. Diagnostics with severity exceeding
// maxSeverity (higher number = less severe) are excluded.
func formatDiagnosticsResponse(
	absPath string,
	wsRoot string,
	fileContent []byte,
	diagnostics []cache.Diagnostic,
	stale bool,
	maxSeverity int,
) DiagnosticsResponse {
	relPath, err := filepath.Rel(wsRoot, absPath)
	if err != nil {
		relPath = absPath
	}

	var lines []string
	if len(fileContent) > 0 {
		lines = strings.Split(string(fileContent), "\n")
	}

	enriched := make([]EnrichedDiagnostic, 0, len(diagnostics))
	errorCount := 0
	warningCount := 0

	for _, diag := range diagnostics {
		if severityValue(diag.Severity) > maxSeverity {
			continue
		}

		severity := severityString(diag.Severity)
		switch severity {
		case "error":
			errorCount++
		case "warning":
			warningCount++
		}

		startLine := diag.Range.Start.Line + 1 // 1-indexed
		endLine := diag.Range.End.Line + 1

		enrichedDiag := EnrichedDiagnostic{
			Line:     startLine,
			Severity: severity,
			Source:   diag.Source,
			Code:     diag.Code,
			Message:  diag.Message,
			Context:  buildContext(lines, diag),
		}
		if endLine != startLine {
			enrichedDiag.EndLine = endLine
		}

		enriched = append(enriched, enrichedDiag)
	}

	return DiagnosticsResponse{
		File:         relPath,
		Diagnostics:  enriched,
		Stale:        stale,
		ErrorCount:   errorCount,
		WarningCount: warningCount,
	}
}

// buildContext produces the annotated code context lines for a diagnostic.
func buildContext(lines []string, diag cache.Diagnostic) []string {
	if len(lines) == 0 {
		return []string{}
	}

	startLine := diag.Range.Start.Line // 0-indexed
	if startLine < 0 || startLine >= len(lines) {
		return []string{}
	}

	// Determine the range of line numbers to display for width calculation.
	lastDisplayLine := startLine + 1 // line below (0-indexed)
	if lastDisplayLine >= len(lines) {
		lastDisplayLine = startLine
	}
	lineNumWidth := len(fmt.Sprintf("%d", lastDisplayLine+1)) // +1 for 1-indexed

	var context []string

	// Line above.
	if startLine > 0 {
		context = append(context, formatCodeLine(lineNumWidth, startLine, lines[startLine-1]))
	}

	// The diagnostic line.
	context = append(context, formatCodeLine(lineNumWidth, startLine+1, lines[startLine]))

	// Pointer line.
	context = append(context, buildPointerLine(lineNumWidth, diag))

	// Line below.
	if startLine+1 < len(lines) {
		context = append(context, formatCodeLine(lineNumWidth, startLine+2, lines[startLine+1]))
	}

	return context
}

// formatCodeLine formats a single line with right-aligned line number.
func formatCodeLine(width int, lineNum int, content string) string {
	return fmt.Sprintf("%*d | %s", width, lineNum, content)
}

// buildPointerLine creates the caret pointer annotation line.
func buildPointerLine(lineNumWidth int, diag cache.Diagnostic) string {
	startCol := diag.Range.Start.Character
	if startCol < 0 {
		startCol = 0
	}
	endCol := diag.Range.End.Character

	spanWidth := endCol - startCol
	if spanWidth < 3 {
		spanWidth = 3
	}

	padding := strings.Repeat(" ", lineNumWidth)
	colPadding := strings.Repeat(" ", startCol)
	carets := strings.Repeat("^", spanWidth)

	message := diag.Message
	if len(message) > contextMessageMaxLen {
		message = message[:contextMessageMaxLen-3] + "..."
	}

	return fmt.Sprintf("%s | %s%s %s", padding, colPadding, carets, message)
}

// DefinitionLocation is a single go-to-definition result with human-readable
// fields: relative path, 1-indexed line/character, and the source line.
type DefinitionLocation struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Context   string `json:"context"`
}

// formatDefinitionResponse converts LSP Location results to CLI-friendly
// DefinitionLocation values with relative paths, 1-indexed positions, and
// the source line at the definition site.
func formatDefinitionResponse(locations []protocol.Location, wsRoot string) DefinitionResponse {
	result := make([]DefinitionLocation, 0, len(locations))
	for _, loc := range locations {
		absPath := strings.TrimPrefix(loc.Uri, "file://")

		relPath, err := filepath.Rel(wsRoot, absPath)
		if err != nil {
			relPath = absPath
		}

		line := int(loc.Range.Start.Line) + 1
		character := int(loc.Range.Start.Character) + 1

		contextLine := readSourceLine(absPath, int(loc.Range.Start.Line))

		result = append(result, DefinitionLocation{
			File:      relPath,
			Line:      line,
			Character: character,
			Context:   contextLine,
		})
	}
	return DefinitionResponse{Locations: result}
}

// readSourceLine reads a single 0-indexed line from a file. Returns empty
// string on any error.
func readSourceLine(absPath string, lineIndex int) string {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if lineIndex < 0 || lineIndex >= len(lines) {
		return ""
	}
	return lines[lineIndex]
}
