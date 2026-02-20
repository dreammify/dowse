package session

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/dreammify/dowse/internal/lsp/protocol"
)

func makeProgressParams(token any, value any) protocol.ProgressParams {
	tokenJSON, _ := json.Marshal(token)
	valueJSON, _ := json.Marshal(value)
	return protocol.ProgressParams{
		Token: tokenJSON,
		Value: valueJSON,
	}
}

func TestProgressTrackerBeginReportEnd(t *testing.T) {
	tracker := NewProgressTracker()

	// Initially idle.
	if status := tracker.Status(); status != "" {
		t.Fatalf("expected empty status, got %q", status)
	}

	// Register and begin.
	tracker.RegisterToken("token-1")
	tracker.HandleProgress(makeProgressParams("token-1", map[string]any{
		"kind":       "begin",
		"title":      "Indexing",
		"message":    "Loading modules",
		"percentage": 10,
	}))

	status, percent := tracker.StatusWithPercent()
	if status != "Indexing - Loading modules" {
		t.Fatalf("expected 'Indexing - Loading modules', got %q", status)
	}
	if percent == nil || *percent != 10 {
		t.Fatalf("expected percent 10, got %v", percent)
	}

	// Report update.
	tracker.HandleProgress(makeProgressParams("token-1", map[string]any{
		"kind":       "report",
		"message":    "Resolving dependencies",
		"percentage": 50,
	}))

	status, percent = tracker.StatusWithPercent()
	if status != "Indexing - Resolving dependencies" {
		t.Fatalf("expected 'Indexing - Resolving dependencies', got %q", status)
	}
	if percent == nil || *percent != 50 {
		t.Fatalf("expected percent 50, got %v", percent)
	}

	// End.
	tracker.HandleProgress(makeProgressParams("token-1", map[string]any{
		"kind": "end",
	}))

	if status := tracker.Status(); status != "" {
		t.Fatalf("expected empty status after end, got %q", status)
	}
}

func TestProgressTrackerIntegerToken(t *testing.T) {
	tracker := NewProgressTracker()

	// Integer token (some servers use integer tokens).
	tracker.RegisterToken("42")
	tracker.HandleProgress(makeProgressParams(42, map[string]any{
		"kind":  "begin",
		"title": "Building",
	}))

	status := tracker.Status()
	if status != "Building" {
		t.Fatalf("expected 'Building', got %q", status)
	}
}

func TestProgressTrackerNoMessage(t *testing.T) {
	tracker := NewProgressTracker()

	tracker.RegisterToken("tok")
	tracker.HandleProgress(makeProgressParams("tok", map[string]any{
		"kind":  "begin",
		"title": "Compiling",
	}))

	status := tracker.Status()
	if status != "Compiling" {
		t.Fatalf("expected 'Compiling', got %q", status)
	}
}

func TestProgressTrackerWithoutCreate(t *testing.T) {
	tracker := NewProgressTracker()

	// Server sends progress without prior create (spec allows this).
	tracker.HandleProgress(makeProgressParams("surprise", map[string]any{
		"kind":  "begin",
		"title": "Analyzing",
	}))

	status := tracker.Status()
	if status != "Analyzing" {
		t.Fatalf("expected 'Analyzing', got %q", status)
	}
}

func TestProgressTrackerConcurrency(t *testing.T) {
	tracker := NewProgressTracker()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := json.RawMessage(`"concurrent"`)
			if i%3 == 0 {
				tracker.RegisterToken("concurrent")
			} else if i%3 == 1 {
				tracker.HandleProgress(protocol.ProgressParams{
					Token: token,
					Value: json.RawMessage(`{"kind":"begin","title":"Work"}`),
				})
			} else {
				_ = tracker.Status()
			}
		}(i)
	}
	wg.Wait()
}

func TestProgressTrackerReportWithoutPercent(t *testing.T) {
	tracker := NewProgressTracker()

	tracker.RegisterToken("tok")
	tracker.HandleProgress(makeProgressParams("tok", map[string]any{
		"kind":       "begin",
		"title":      "Setup",
		"percentage": 25,
	}))

	// Report without percentage clears it.
	tracker.HandleProgress(makeProgressParams("tok", map[string]any{
		"kind":    "report",
		"message": "Almost done",
	}))

	status, percent := tracker.StatusWithPercent()
	if status != "Setup - Almost done" {
		t.Fatalf("expected 'Setup - Almost done', got %q", status)
	}
	if percent != nil {
		t.Fatalf("expected nil percent after report without percentage, got %v", *percent)
	}
}
