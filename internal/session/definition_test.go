package session

import (
	"encoding/json"
	"testing"
)

func TestParseDefinitionResult_Null(t *testing.T) {
	result, err := parseDefinitionResult(json.RawMessage("null"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty slice for null, got %d", len(result))
	}
}

func TestParseDefinitionResult_Empty(t *testing.T) {
	result, err := parseDefinitionResult(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty slice for empty input, got %d", len(result))
	}
}

func TestParseDefinitionResult_SingleLocation(t *testing.T) {
	raw := json.RawMessage(`{"uri":"file:///tmp/test.go","range":{"start":{"line":5,"character":3},"end":{"line":5,"character":10}}}`)
	result, err := parseDefinitionResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 location, got %d", len(result))
	}
	if result[0].Uri != "file:///tmp/test.go" {
		t.Errorf("expected uri 'file:///tmp/test.go', got %q", result[0].Uri)
	}
	if result[0].Range.Start.Line != 5 {
		t.Errorf("expected start line 5, got %d", result[0].Range.Start.Line)
	}
	if result[0].Range.Start.Character != 3 {
		t.Errorf("expected start character 3, got %d", result[0].Range.Start.Character)
	}
}

func TestParseDefinitionResult_Array(t *testing.T) {
	raw := json.RawMessage(`[{"uri":"file:///a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}}},{"uri":"file:///b.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":8}}}]`)
	result, err := parseDefinitionResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(result))
	}
	if result[0].Uri != "file:///a.go" {
		t.Errorf("expected first uri 'file:///a.go', got %q", result[0].Uri)
	}
	if result[1].Uri != "file:///b.go" {
		t.Errorf("expected second uri 'file:///b.go', got %q", result[1].Uri)
	}
}

func TestParseDefinitionResult_EmptyArray(t *testing.T) {
	raw := json.RawMessage(`[]`)
	result, err := parseDefinitionResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 locations, got %d", len(result))
	}
}

func TestParseDefinitionResult_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`invalid`)
	_, err := parseDefinitionResult(raw)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
