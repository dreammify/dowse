package main

import (
	"strings"
	"testing"

	"github.com/dreammify/dowse/internal/daemon"
)

func TestFormatDiagnosticsTextSingleError(t *testing.T) {
	resp := daemon.DiagnosticsResponse{
		File: "cmd/main.go",
		Diagnostics: []daemon.EnrichedDiagnostic{
			{
				Line:     6,
				Severity: "error",
				Source:   "gopls",
				Code:     "UndeclaredName",
				Message:  "undeclared name: undefined",
				Context: []string{
					"5 | func main() {",
					"6 | \tfmt.Println(undefined)",
					"  |              ^^^^^^^^^ undeclared name: undefined",
					"7 | }",
				},
			},
		},
		ErrorCount: 1,
	}

	output := formatDiagnosticsText(resp)

	if !strings.Contains(output, "✘ [Line 6] undeclared name: undefined") {
		t.Errorf("expected error icon and line header, got:\n%s", output)
	}
	if !strings.Contains(output, "    6 | \tfmt.Println(undefined)") {
		t.Errorf("expected indented code context, got:\n%s", output)
	}
	if !strings.Contains(output, "    "+`  |              ^^^^^^^^^ undeclared name: undefined`) {
		t.Errorf("expected indented pointer line, got:\n%s", output)
	}
}

func TestFormatDiagnosticsTextWarning(t *testing.T) {
	resp := daemon.DiagnosticsResponse{
		File: "file.go",
		Diagnostics: []daemon.EnrichedDiagnostic{
			{
				Line:     3,
				Severity: "warning",
				Message:  "unused variable",
				Context:  []string{"3 | x := 1"},
			},
		},
		WarningCount: 1,
	}

	output := formatDiagnosticsText(resp)

	if !strings.Contains(output, "⚠ [Line 3] unused variable") {
		t.Errorf("expected warning icon, got:\n%s", output)
	}
}

func TestFormatDiagnosticsTextEmpty(t *testing.T) {
	resp := daemon.DiagnosticsResponse{
		File:        "file.go",
		Diagnostics: []daemon.EnrichedDiagnostic{},
	}

	output := formatDiagnosticsText(resp)
	if output != "" {
		t.Errorf("expected empty output for no diagnostics, got:\n%q", output)
	}
}

func TestFormatDiagnosticsTextMultiple(t *testing.T) {
	resp := daemon.DiagnosticsResponse{
		File: "file.go",
		Diagnostics: []daemon.EnrichedDiagnostic{
			{
				Line:     8,
				Severity: "error",
				Message:  "Unresolved reference 'DoesNotExist'.",
				Context: []string{
					"8 | val x = DoesNotExist",
					"  |         ^^^^^^^^^^^^ Unresolved reference 'DoesNotExist'.",
				},
			},
			{
				Line:     16,
				Severity: "error",
				Message:  "No value passed for parameter 'broken'.",
				Context: []string{
					"15 |             email = claims.email,",
					"16 |             fid = claims.flexportUserFid?.let { Fid.parse(it) },",
					"   |             ^^^ No value passed for parameter 'broken'.",
					"17 |         )",
				},
			},
		},
		ErrorCount: 2,
	}

	output := formatDiagnosticsText(resp)

	// Both diagnostics should be present.
	if strings.Count(output, "✘") != 2 {
		t.Errorf("expected 2 error icons, got:\n%s", output)
	}
	if !strings.Contains(output, "[Line 8]") {
		t.Errorf("expected Line 8 header, got:\n%s", output)
	}
	if !strings.Contains(output, "[Line 16]") {
		t.Errorf("expected Line 16 header, got:\n%s", output)
	}
}

func TestFormatBatchDiagnosticsText(t *testing.T) {
	resp := daemon.BatchDiagnosticsResponse{
		Files: []daemon.DiagnosticsResponse{
			{
				File: "src/a.go",
				Diagnostics: []daemon.EnrichedDiagnostic{
					{Line: 5, Severity: "error", Message: "err1", Context: []string{"5 | bad"}},
				},
				ErrorCount: 1,
			},
			{
				File:        "src/b.go",
				Diagnostics: []daemon.EnrichedDiagnostic{},
			},
			{
				File: "src/c.go",
				Diagnostics: []daemon.EnrichedDiagnostic{
					{Line: 10, Severity: "warning", Message: "warn1", Context: []string{"10 | meh"}},
				},
				WarningCount: 1,
			},
		},
		TotalErrors:   1,
		TotalWarnings: 1,
	}

	output := formatBatchDiagnosticsText(resp)

	// File headers for files with diagnostics.
	if !strings.Contains(output, "src/a.go") {
		t.Errorf("expected src/a.go header, got:\n%s", output)
	}
	if !strings.Contains(output, "src/c.go") {
		t.Errorf("expected src/c.go header, got:\n%s", output)
	}
	// File with no diagnostics should be skipped.
	if strings.Contains(output, "src/b.go") {
		t.Errorf("expected src/b.go to be omitted, got:\n%s", output)
	}
	// Summary line.
	if !strings.Contains(output, "Found 1 error(s), 1 warning(s).") {
		t.Errorf("expected summary line, got:\n%s", output)
	}
}

func TestFormatBatchDiagnosticsTextNoIssues(t *testing.T) {
	resp := daemon.BatchDiagnosticsResponse{
		Files: []daemon.DiagnosticsResponse{
			{File: "a.go", Diagnostics: []daemon.EnrichedDiagnostic{}},
		},
	}

	output := formatBatchDiagnosticsText(resp)
	if output != "" {
		t.Errorf("expected empty output for no issues, got:\n%q", output)
	}
}

func TestSeverityIcons(t *testing.T) {
	tests := []struct {
		severity string
		icon     string
	}{
		{"error", "✘"},
		{"warning", "⚠"},
		{"information", "ℹ"},
		{"hint", "•"},
		{"unknown", "✘"},
	}
	for _, testCase := range tests {
		got := severityIcon(testCase.severity)
		if got != testCase.icon {
			t.Errorf("severityIcon(%q) = %q, want %q", testCase.severity, got, testCase.icon)
		}
	}
}
