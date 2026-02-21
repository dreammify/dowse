package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dreammify/dowse/internal/cache"
	"github.com/dreammify/dowse/internal/lsp/protocol"
)

func intPtr(value int) *int { return &value }

func TestFormatBasic(t *testing.T) {
	content := []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(undefined)\n}\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 5, Character: 14},
				End:   cache.Position{Line: 5, Character: 23},
			},
			Severity: intPtr(1),
			Source:   "gopls",
			Code:     "UndeclaredName",
			Message:  "undeclared name: undefined",
		},
	}

	resp := formatDiagnosticsResponse("/workspace/cmd/main.go", "/workspace", content, diags, false, 4)

	if resp.File != "cmd/main.go" {
		t.Fatalf("expected relative path 'cmd/main.go', got %q", resp.File)
	}
	if len(resp.Diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(resp.Diagnostics))
	}

	diag := resp.Diagnostics[0]
	if diag.Line != 6 {
		t.Errorf("expected line 6 (1-indexed), got %d", diag.Line)
	}
	if diag.EndLine != 0 {
		t.Errorf("expected end_line 0 (same as start), got %d", diag.EndLine)
	}
	if diag.Severity != "error" {
		t.Errorf("expected severity 'error', got %q", diag.Severity)
	}
	if diag.Source != "gopls" {
		t.Errorf("expected source 'gopls', got %q", diag.Source)
	}
	if diag.Code != "UndeclaredName" {
		t.Errorf("expected code 'UndeclaredName', got %q", diag.Code)
	}
	if diag.Message != "undeclared name: undefined" {
		t.Errorf("expected message 'undeclared name: undefined', got %q", diag.Message)
	}
	if len(diag.Context) != 4 {
		t.Fatalf("expected 4 context lines (above, diag, pointer, below), got %d: %v", len(diag.Context), diag.Context)
	}
	// Verify pointer line contains carets.
	if !strings.Contains(diag.Context[2], "^^^") {
		t.Errorf("expected pointer line with carets, got %q", diag.Context[2])
	}
	if resp.ErrorCount != 1 {
		t.Errorf("expected error_count 1, got %d", resp.ErrorCount)
	}
	if resp.WarningCount != 0 {
		t.Errorf("expected warning_count 0, got %d", resp.WarningCount)
	}
	if resp.Stale {
		t.Error("expected stale=false")
	}
}

func TestFormatMultiLineDiagnostic(t *testing.T) {
	content := []byte("line0\nline1\nline2\nline3\nline4\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 1, Character: 0},
				End:   cache.Position{Line: 3, Character: 5},
			},
			Severity: intPtr(1),
			Message:  "multi-line error",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	if diag.Line != 2 {
		t.Errorf("expected line 2, got %d", diag.Line)
	}
	if diag.EndLine != 4 {
		t.Errorf("expected end_line 4, got %d", diag.EndLine)
	}
	// Context should annotate the start line only.
	if !strings.Contains(diag.Context[1], "line1") {
		t.Errorf("expected context to show start line 'line1', got %v", diag.Context)
	}
}

func TestFormatFirstLineOfFile(t *testing.T) {
	content := []byte("first line\nsecond line\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 0, Character: 0},
				End:   cache.Position{Line: 0, Character: 5},
			},
			Severity: intPtr(1),
			Message:  "error on first line",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	// No line above, so context should be: diag line, pointer, line below = 3 lines.
	if len(diag.Context) != 3 {
		t.Fatalf("expected 3 context lines (no line above), got %d: %v", len(diag.Context), diag.Context)
	}
	if !strings.Contains(diag.Context[0], "first line") {
		t.Errorf("expected first context line to be the diagnostic line, got %q", diag.Context[0])
	}
}

func TestFormatLastLineOfFile(t *testing.T) {
	content := []byte("first line\nlast line")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 1, Character: 0},
				End:   cache.Position{Line: 1, Character: 4},
			},
			Severity: intPtr(2),
			Message:  "warning on last line",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	// Line above, diag line, pointer, no line below = 3 lines.
	if len(diag.Context) != 3 {
		t.Fatalf("expected 3 context lines (no line below), got %d: %v", len(diag.Context), diag.Context)
	}
	if resp.WarningCount != 1 {
		t.Errorf("expected warning_count 1, got %d", resp.WarningCount)
	}
}

func TestFormatEmptyDiagnostics(t *testing.T) {
	content := []byte("some content\n")
	var diags []cache.Diagnostic

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	if resp.Diagnostics == nil {
		t.Fatal("expected empty slice, got nil")
	}
	if len(resp.Diagnostics) != 0 {
		t.Fatalf("expected 0 diagnostics, got %d", len(resp.Diagnostics))
	}
	if resp.ErrorCount != 0 {
		t.Errorf("expected error_count 0, got %d", resp.ErrorCount)
	}
	if resp.WarningCount != 0 {
		t.Errorf("expected warning_count 0, got %d", resp.WarningCount)
	}
}

func TestFormatNilSeverity(t *testing.T) {
	content := []byte("line0\nline1\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 0, Character: 0},
				End:   cache.Position{Line: 0, Character: 5},
			},
			Severity: nil,
			Message:  "nil severity defaults to error",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	if resp.Diagnostics[0].Severity != "error" {
		t.Errorf("expected nil severity to default to 'error', got %q", resp.Diagnostics[0].Severity)
	}
	if resp.ErrorCount != 1 {
		t.Errorf("expected error_count 1 for nil severity, got %d", resp.ErrorCount)
	}
}

func TestFormatSeverityCounts(t *testing.T) {
	content := []byte("a\nb\nc\nd\ne\n")
	diags := []cache.Diagnostic{
		{Range: cache.Range{Start: cache.Position{Line: 0}, End: cache.Position{Line: 0, Character: 1}}, Severity: intPtr(1), Message: "err1"},
		{Range: cache.Range{Start: cache.Position{Line: 1}, End: cache.Position{Line: 1, Character: 1}}, Severity: intPtr(2), Message: "warn1"},
		{Range: cache.Range{Start: cache.Position{Line: 2}, End: cache.Position{Line: 2, Character: 1}}, Severity: intPtr(1), Message: "err2"},
		{Range: cache.Range{Start: cache.Position{Line: 3}, End: cache.Position{Line: 3, Character: 1}}, Severity: intPtr(3), Message: "info1"},
		{Range: cache.Range{Start: cache.Position{Line: 4}, End: cache.Position{Line: 4, Character: 1}}, Severity: intPtr(4), Message: "hint1"},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	if resp.ErrorCount != 2 {
		t.Errorf("expected error_count 2, got %d", resp.ErrorCount)
	}
	if resp.WarningCount != 1 {
		t.Errorf("expected warning_count 1, got %d", resp.WarningCount)
	}
	if resp.Diagnostics[3].Severity != "information" {
		t.Errorf("expected 'information', got %q", resp.Diagnostics[3].Severity)
	}
	if resp.Diagnostics[4].Severity != "hint" {
		t.Errorf("expected 'hint', got %q", resp.Diagnostics[4].Severity)
	}
}

func TestFormatSourceAndCode(t *testing.T) {
	content := []byte("x\n")
	diags := []cache.Diagnostic{
		{
			Range:    cache.Range{Start: cache.Position{Line: 0}, End: cache.Position{Line: 0, Character: 1}},
			Severity: intPtr(1),
			Source:   "myLinter",
			Code:     "LINT001",
			Message:  "lint error",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	if diag.Source != "myLinter" {
		t.Errorf("expected source 'myLinter', got %q", diag.Source)
	}
	if diag.Code != "LINT001" {
		t.Errorf("expected code 'LINT001', got %q", diag.Code)
	}
}

func TestFormatLongMessageTruncation(t *testing.T) {
	content := []byte("x\n")
	longMsg := strings.Repeat("a", 100)
	diags := []cache.Diagnostic{
		{
			Range:    cache.Range{Start: cache.Position{Line: 0}, End: cache.Position{Line: 0, Character: 1}},
			Severity: intPtr(1),
			Message:  longMsg,
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	// Full message should be preserved.
	if diag.Message != longMsg {
		t.Error("full message should be preserved in message field")
	}
	// Pointer line message should be truncated.
	pointerLine := diag.Context[1] // no line above for first line, so [0]=diag, [1]=pointer
	if len(pointerLine) > 200 {
		t.Errorf("pointer line should have truncated message, but is very long: %d chars", len(pointerLine))
	}
	if !strings.Contains(pointerLine, "...") {
		t.Errorf("expected truncated pointer message to end with '...', got %q", pointerLine)
	}
}

func TestFormatRelativePath(t *testing.T) {
	content := []byte("x\n")
	diags := []cache.Diagnostic{}

	resp := formatDiagnosticsResponse("/home/user/project/src/main.go", "/home/user/project", content, diags, false, 4)

	if resp.File != "src/main.go" {
		t.Errorf("expected 'src/main.go', got %q", resp.File)
	}
}

func TestFormatStaleFlag(t *testing.T) {
	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", []byte("x\n"), nil, true, 4)

	if !resp.Stale {
		t.Error("expected stale=true")
	}
	if resp.Diagnostics == nil {
		t.Fatal("expected empty slice, got nil")
	}
}

func TestFormatEmptyFileContent(t *testing.T) {
	diags := []cache.Diagnostic{
		{
			Range:    cache.Range{Start: cache.Position{Line: 0}, End: cache.Position{Line: 0, Character: 5}},
			Severity: intPtr(1),
			Message:  "error in empty file",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", nil, diags, false, 4)

	diag := resp.Diagnostics[0]
	if len(diag.Context) != 0 {
		t.Errorf("expected empty context for nil file content, got %v", diag.Context)
	}
}

func TestFormatPointerLineAlignment(t *testing.T) {
	// Verify the pointer line aligns correctly with the code.
	content := []byte("abcdefghij\n1234567890\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 1, Character: 3},
				End:   cache.Position{Line: 1, Character: 7},
			},
			Severity: intPtr(2),
			Message:  "bad range",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	// Context: line above (line 1), diagnostic line (line 2), pointer, (no line below since "1234567890" is last non-empty)
	pointerLine := diag.Context[2]
	// Should have spaces for line number area + " | " + 3 spaces + 4 carets.
	if !strings.Contains(pointerLine, "   ^^^^") {
		t.Errorf("expected pointer with 3 space padding and 4 carets, got %q", pointerLine)
	}
	if !strings.Contains(pointerLine, "bad range") {
		t.Errorf("expected message in pointer line, got %q", pointerLine)
	}
}

func TestPointerLineCaretAlignmentWithCodeLine(t *testing.T) {
	// Regression test: the pointer line's carets must align with the exact
	// characters in the code line. Previously the format string in
	// buildPointerLine was missing a space after "|", shifting carets one
	// column to the left relative to formatCodeLine's output.
	content := []byte("abcdefghij\n0123456789\nklmnopqrst\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 1, Character: 4},
				End:   cache.Position{Line: 1, Character: 7},
			},
			Severity: intPtr(1),
			Message:  "bad value",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)
	diag := resp.Diagnostics[0]

	// Context: line above (line 1), diagnostic line (line 2), pointer, line below (line 3).
	if len(diag.Context) != 4 {
		t.Fatalf("expected 4 context lines, got %d: %v", len(diag.Context), diag.Context)
	}

	codeLine := diag.Context[1]    // "2 | 0123456789"
	pointerLine := diag.Context[2] // "  |     ^^^ bad value"

	// Find the position of the pipe separator in both lines.
	codePipeIdx := strings.Index(codeLine, "|")
	pointerPipeIdx := strings.Index(pointerLine, "|")
	if codePipeIdx < 0 || pointerPipeIdx < 0 {
		t.Fatalf("expected pipe in both lines, code=%q pointer=%q", codeLine, pointerLine)
	}

	// After the pipe, the code line has " 0123456789" (space + content).
	// The pointer line must also have a space after the pipe before padding.
	codeAfterPipe := codeLine[codePipeIdx+1:]
	pointerAfterPipe := pointerLine[pointerPipeIdx+1:]

	if len(codeAfterPipe) == 0 || codeAfterPipe[0] != ' ' {
		t.Fatalf("expected space after pipe in code line, got %q", codeAfterPipe)
	}
	if len(pointerAfterPipe) == 0 || pointerAfterPipe[0] != ' ' {
		t.Fatalf("expected space after pipe in pointer line, got %q", pointerAfterPipe)
	}

	// The caret should start at the same visual column as the character at
	// position 4 in the code content. Find the column of '4' in the code line
	// and '^' in the pointer line; they must match.
	codeContentStart := codePipeIdx + 2 // skip "| "
	caretIdx := strings.Index(pointerLine, "^")
	if caretIdx < 0 {
		t.Fatalf("expected carets in pointer line, got %q", pointerLine)
	}

	expectedCaretCol := codeContentStart + 4 // Character: 4
	if caretIdx != expectedCaretCol {
		t.Errorf("caret column mismatch: caret at column %d, expected %d (code=%q, pointer=%q)",
			caretIdx, expectedCaretCol, codeLine, pointerLine)
	}
}

// TestBuildPointerLineNegativeStartCol is a regression test for a panic when an
// LSP server sends a diagnostic with a negative Character value. The startCol
// must be clamped to >= 0 to avoid a panic in strings.Repeat.
func TestBuildPointerLineNegativeStartCol(t *testing.T) {
	content := []byte("hello world\n")
	diags := []cache.Diagnostic{
		{
			Range: cache.Range{
				Start: cache.Position{Line: 0, Character: -1},
				End:   cache.Position{Line: 0, Character: 5},
			},
			Severity: intPtr(1),
			Message:  "negative col",
		},
	}

	resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, 4)

	diag := resp.Diagnostics[0]
	// Should not panic and should produce a valid pointer line.
	found := false
	for _, line := range diag.Context {
		if strings.Contains(line, "^^^") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pointer line with carets, got %v", diag.Context)
	}
}

func TestConvertDiagnosticsCodePolymorphism(t *testing.T) {
	stringSource := "gopls"
	tests := []struct {
		name     string
		code     json.RawMessage
		expected string
	}{
		{"string code", json.RawMessage(`"UndeclaredName"`), "UndeclaredName"},
		{"integer code", json.RawMessage(`42`), "42"},
		{"missing code", nil, ""},
		{"empty code", json.RawMessage(``), ""},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			sev := protocol.DiagnosticSeverityError
			protos := []protocol.Diagnostic{
				{
					Range: protocol.Range{
						Start: protocol.Position{Line: 0, Character: 0},
						End:   protocol.Position{Line: 0, Character: 5},
					},
					Severity: &sev,
					Source:   &stringSource,
					Code:     testCase.code,
					Message:  "test",
				},
			}

			result := convertDiagnostics(protos)

			if len(result) != 1 {
				t.Fatalf("expected 1 diagnostic, got %d", len(result))
			}
			if result[0].Code != testCase.expected {
				t.Errorf("expected code %q, got %q", testCase.expected, result[0].Code)
			}
		})
	}
}

func TestConvertDiagnosticsSourceAndTags(t *testing.T) {
	source := "myLinter"
	sev := protocol.DiagnosticSeverityWarning
	protos := []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 1, Character: 0},
				End:   protocol.Position{Line: 1, Character: 10},
			},
			Severity: &sev,
			Source:   &source,
			Tags:     []protocol.DiagnosticTag{protocol.DiagnosticTagUnnecessary, protocol.DiagnosticTagDeprecated},
			Message:  "unused and deprecated",
		},
	}

	result := convertDiagnostics(protos)

	if result[0].Source != "myLinter" {
		t.Errorf("expected source 'myLinter', got %q", result[0].Source)
	}
	if len(result[0].Tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(result[0].Tags))
	}
	if result[0].Tags[0] != 1 || result[0].Tags[1] != 2 {
		t.Errorf("expected tags [1, 2], got %v", result[0].Tags)
	}
}

func TestConvertDiagnosticsNilSource(t *testing.T) {
	sev := protocol.DiagnosticSeverityError
	protos := []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 0, Character: 1},
			},
			Severity: &sev,
			Source:   nil,
			Message:  "no source",
		},
	}

	result := convertDiagnostics(protos)

	if result[0].Source != "" {
		t.Errorf("expected empty source for nil, got %q", result[0].Source)
	}
}

func TestFormatDefinitionResponse_RelativePathAndIndexing(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "src", "main.go")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetFile, []byte("package main\n\nfunc hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	locations := []protocol.Location{
		{
			Uri: "file://" + targetFile,
			Range: protocol.Range{
				Start: protocol.Position{Line: 2, Character: 5},
				End:   protocol.Position{Line: 2, Character: 10},
			},
		},
	}

	resp := formatDefinitionResponse(locations, dir)

	if len(resp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(resp.Locations))
	}

	loc := resp.Locations[0]
	if loc.File != "src/main.go" {
		t.Errorf("expected relative path 'src/main.go', got %q", loc.File)
	}
	if loc.Line != 3 {
		t.Errorf("expected 1-indexed line 3, got %d", loc.Line)
	}
	if loc.Character != 6 {
		t.Errorf("expected 1-indexed character 6, got %d", loc.Character)
	}
	if loc.Context != "func hello() {}" {
		t.Errorf("expected context 'func hello() {}', got %q", loc.Context)
	}
}

func TestFormatDefinitionResponse_Empty(t *testing.T) {
	resp := formatDefinitionResponse(nil, "/ws")
	if resp.Locations == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(resp.Locations) != 0 {
		t.Fatalf("expected 0 locations, got %d", len(resp.Locations))
	}
}

func TestFormatDefinitionResponse_FileOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	outsideFile := filepath.Join(dir, "stdlib.go")
	if err := os.WriteFile(outsideFile, []byte("package fmt\n\nfunc Println() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	locations := []protocol.Location{
		{
			Uri: "file://" + outsideFile,
			Range: protocol.Range{
				Start: protocol.Position{Line: 2, Character: 5},
				End:   protocol.Position{Line: 2, Character: 12},
			},
		},
	}

	resp := formatDefinitionResponse(locations, "/completely/different/workspace")

	if len(resp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(resp.Locations))
	}
	// When outside workspace, filepath.Rel produces a ../ relative path.
	// Verify the location still has correct 1-indexed coordinates and context.
	loc := resp.Locations[0]
	if loc.Line != 3 {
		t.Errorf("expected 1-indexed line 3, got %d", loc.Line)
	}
	if loc.Context != "func Println() {}" {
		t.Errorf("expected context 'func Println() {}', got %q", loc.Context)
	}
}

func TestFormatDefinitionResponse_ContextLineUnreadable(t *testing.T) {
	locations := []protocol.Location{
		{
			Uri: "file:///nonexistent/path.go",
			Range: protocol.Range{
				Start: protocol.Position{Line: 5, Character: 0},
				End:   protocol.Position{Line: 5, Character: 10},
			},
		},
	}

	resp := formatDefinitionResponse(locations, "/ws")

	if len(resp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(resp.Locations))
	}
	if resp.Locations[0].Context != "" {
		t.Errorf("expected empty context for unreadable file, got %q", resp.Locations[0].Context)
	}
}

func TestFormatDefinitionResponse_LineOutOfRange(t *testing.T) {
	dir := t.TempDir()
	shortFile := filepath.Join(dir, "short.go")
	// File has only 2 lines (0 and 1).
	if err := os.WriteFile(shortFile, []byte("line0\nline1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	locations := []protocol.Location{
		{
			Uri: "file://" + shortFile,
			Range: protocol.Range{
				Start: protocol.Position{Line: 99, Character: 0},
				End:   protocol.Position{Line: 99, Character: 5},
			},
		},
	}

	resp := formatDefinitionResponse(locations, dir)
	if len(resp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(resp.Locations))
	}
	if resp.Locations[0].Context != "" {
		t.Errorf("expected empty context for out-of-range line, got %q", resp.Locations[0].Context)
	}
	// Line should still be 1-indexed conversion of 99 -> 100.
	if resp.Locations[0].Line != 100 {
		t.Errorf("expected line 100, got %d", resp.Locations[0].Line)
	}
}

func TestFormatSeverityFiltering(t *testing.T) {
	content := []byte("a\nb\nc\nd\ne\n")
	diags := []cache.Diagnostic{
		{Range: cache.Range{Start: cache.Position{Line: 0}, End: cache.Position{Line: 0, Character: 1}}, Severity: intPtr(1), Message: "err"},
		{Range: cache.Range{Start: cache.Position{Line: 1}, End: cache.Position{Line: 1, Character: 1}}, Severity: intPtr(2), Message: "warn"},
		{Range: cache.Range{Start: cache.Position{Line: 2}, End: cache.Position{Line: 2, Character: 1}}, Severity: intPtr(3), Message: "info"},
		{Range: cache.Range{Start: cache.Position{Line: 3}, End: cache.Position{Line: 3, Character: 1}}, Severity: intPtr(4), Message: "hint"},
		{Range: cache.Range{Start: cache.Position{Line: 4}, End: cache.Position{Line: 4, Character: 1}}, Severity: nil, Message: "nil-sev"},
	}

	tests := []struct {
		name         string
		maxSeverity  int
		wantCount    int
		wantErrors   int
		wantWarnings int
	}{
		{"error only", 1, 2, 2, 0},  // error + nil (treated as error)
		{"warning", 2, 3, 2, 1},     // error + warning + nil
		{"information", 3, 4, 2, 1}, // error + warning + information + nil
		{"hint (all)", 4, 5, 2, 1},  // all diagnostics
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resp := formatDiagnosticsResponse("/ws/file.go", "/ws", content, diags, false, testCase.maxSeverity)
			if len(resp.Diagnostics) != testCase.wantCount {
				t.Errorf("expected %d diagnostics, got %d", testCase.wantCount, len(resp.Diagnostics))
			}
			if resp.ErrorCount != testCase.wantErrors {
				t.Errorf("expected %d errors, got %d", testCase.wantErrors, resp.ErrorCount)
			}
			if resp.WarningCount != testCase.wantWarnings {
				t.Errorf("expected %d warnings, got %d", testCase.wantWarnings, resp.WarningCount)
			}
		})
	}
}
