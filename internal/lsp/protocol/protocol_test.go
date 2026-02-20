package protocol

import (
	"encoding/json"
	"testing"
)

func TestDiagnosticRoundTrip(t *testing.T) {
	input := `{
		"range": {
			"start": {"line": 10, "character": 4},
			"end": {"line": 10, "character": 10}
		},
		"severity": 1,
		"message": "undefined: bar",
		"source": "gopls"
	}`

	var diag Diagnostic
	if err := json.Unmarshal([]byte(input), &diag); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if diag.Range.Start.Line != 10 {
		t.Errorf("Range.Start.Line = %d, want 10", diag.Range.Start.Line)
	}
	if diag.Range.Start.Character != 4 {
		t.Errorf("Range.Start.Character = %d, want 4", diag.Range.Start.Character)
	}
	if *diag.Severity != DiagnosticSeverityError {
		t.Errorf("Severity = %v, want %v", *diag.Severity, DiagnosticSeverityError)
	}
	if diag.Message != "undefined: bar" {
		t.Errorf("Message = %q, want %q", diag.Message, "undefined: bar")
	}

	// Re-marshal.
	data, err := json.Marshal(diag)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var diag2 Diagnostic
	if err := json.Unmarshal(data, &diag2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if diag2.Range.Start.Line != diag.Range.Start.Line {
		t.Errorf("round-trip: Start.Line = %d, want %d", diag2.Range.Start.Line, diag.Range.Start.Line)
	}
	if diag2.Message != diag.Message {
		t.Errorf("round-trip: Message = %q, want %q", diag2.Message, diag.Message)
	}
}

func TestInitializeResultRoundTrip(t *testing.T) {
	input := `{
		"capabilities": {
			"textDocumentSync": 1,
			"diagnosticProvider": {
				"interFileDependencies": true,
				"workspaceDiagnostics": false
			}
		}
	}`

	var result InitializeResult
	if err := json.Unmarshal([]byte(input), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// DiagnosticProvider should be non-nil (it was present in the JSON).
	if result.Capabilities.DiagnosticProvider == nil {
		t.Fatal("DiagnosticProvider should be non-nil")
	}

	// Re-marshal and verify round-trip.
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var result2 InitializeResult
	if err := json.Unmarshal(data, &result2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
}

func TestOptionalFieldsOmitted(t *testing.T) {
	diag := Diagnostic{
		Range: Range{
			Start: Position{Line: 1, Character: 0},
			End:   Position{Line: 1, Character: 5},
		},
		Message: "test error",
		// Severity is nil (optional).
	}

	data, err := json.Marshal(diag)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Severity should be omitted from JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, ok := raw["severity"]; ok {
		t.Error("severity should be omitted when nil")
	}
}

func TestOptionalFieldsMissing(t *testing.T) {
	input := `{
		"range": {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 0}},
		"message": "err"
	}`

	var diag Diagnostic
	if err := json.Unmarshal([]byte(input), &diag); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if diag.Severity != nil {
		t.Errorf("Severity should be nil, got %v", *diag.Severity)
	}
}

func TestDiagnosticSeverityValues(t *testing.T) {
	tests := []struct {
		name  string
		value DiagnosticSeverity
		want  uint32
	}{
		{"Error", DiagnosticSeverityError, 1},
		{"Warning", DiagnosticSeverityWarning, 2},
		{"Information", DiagnosticSeverityInformation, 3},
		{"Hint", DiagnosticSeverityHint, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if uint32(tt.value) != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.value, tt.want)
			}
		})
	}
}

func TestPublishDiagnosticsParams(t *testing.T) {
	input := `{
		"uri": "file:///tmp/foo.go",
		"version": 3,
		"diagnostics": [
			{
				"range": {"start": {"line": 5, "character": 0}, "end": {"line": 5, "character": 10}},
				"severity": 1,
				"message": "undefined: x"
			}
		]
	}`

	var params PublishDiagnosticsParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if params.Uri != "file:///tmp/foo.go" {
		t.Errorf("URI = %q, want %q", params.Uri, "file:///tmp/foo.go")
	}
	if *params.Version != 3 {
		t.Errorf("Version = %d, want 3", *params.Version)
	}
	if len(params.Diagnostics) != 1 {
		t.Fatalf("len(Diagnostics) = %d, want 1", len(params.Diagnostics))
	}
	if params.Diagnostics[0].Message != "undefined: x" {
		t.Errorf("Diagnostics[0].Message = %q, want %q", params.Diagnostics[0].Message, "undefined: x")
	}
}

func TestPositionAndRange(t *testing.T) {
	pos := Position{Line: 42, Character: 7}
	data, err := json.Marshal(pos)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var pos2 Position
	if err := json.Unmarshal(data, &pos2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if pos2 != pos {
		t.Errorf("round-trip: got %+v, want %+v", pos2, pos)
	}

	textRange := Range{Start: pos, End: Position{Line: 42, Character: 15}}
	data, err = json.Marshal(textRange)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var textRange2 Range
	if err := json.Unmarshal(data, &textRange2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if textRange2 != textRange {
		t.Errorf("round-trip: got %+v, want %+v", textRange2, textRange)
	}
}
