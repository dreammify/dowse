package session

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dreammify/dowse/internal/lsp/protocol"
)

// progressEntry tracks the state of a single work-done progress token.
type progressEntry struct {
	title      string
	message    string
	percentage *uint32
	done       bool
}

// ProgressTracker tracks LSP work-done progress tokens.
// Thread-safe for concurrent access.
type ProgressTracker struct {
	mu            sync.Mutex
	tokens        map[string]*progressEntry
	serviceStatus string // synthetic status from language/status notifications
}

// NewProgressTracker creates a new ProgressTracker.
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		tokens: make(map[string]*progressEntry),
	}
}

// RegisterToken records a progress token from window/workDoneProgress/create.
func (p *ProgressTracker) RegisterToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens[token] = &progressEntry{}
}

// HandleProgress processes a $/progress notification.
func (p *ProgressTracker) HandleProgress(params protocol.ProgressParams) {
	token := normalizeToken(params.Token)

	// Determine the kind by peeking at the JSON.
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(params.Value, &kind); err != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry, exists := p.tokens[token]
	if !exists {
		// Server may send progress without a prior create request (spec allows it).
		entry = &progressEntry{}
		p.tokens[token] = entry
	}

	switch kind.Kind {
	case "begin":
		var begin protocol.WorkDoneProgressBegin
		if err := json.Unmarshal(params.Value, &begin); err != nil {
			return
		}
		entry.title = begin.Title
		if begin.Message != nil {
			entry.message = *begin.Message
		}
		entry.percentage = begin.Percentage
		entry.done = false

	case "report":
		var report protocol.WorkDoneProgressReport
		if err := json.Unmarshal(params.Value, &report); err != nil {
			return
		}
		if report.Message != nil {
			entry.message = *report.Message
		}
		entry.percentage = report.Percentage

	case "end":
		var end protocol.WorkDoneProgressEnd
		if err := json.Unmarshal(params.Value, &end); err != nil {
			return
		}
		entry.done = true
		// Clean up completed tokens.
		delete(p.tokens, token)
	}
}

// Status returns a human-readable summary of active progress, or empty string if idle.
func (p *ProgressTracker) Status() string {
	status, _ := p.StatusWithPercent()
	return status
}

// StatusWithPercent returns the status string and percentage separately.
// Returns ("", nil) if no active progress.
func (p *ProgressTracker) StatusWithPercent() (string, *uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find the first active (non-done) entry.
	for _, entry := range p.tokens {
		if entry.done {
			continue
		}
		status := entry.title
		if entry.message != "" {
			status = fmt.Sprintf("%s - %s", status, entry.message)
		}
		return status, entry.percentage
	}
	// Fall back to service status from language/status notifications.
	if p.serviceStatus != "" {
		return p.serviceStatus, nil
	}
	return "", nil
}

// SetServiceStatus updates the synthetic service status from language/status
// notifications. "Starting" and "Started" types set the status; "ServiceReady"
// clears it.
func (p *ProgressTracker) SetServiceStatus(statusType, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch statusType {
	case "Starting", "Started":
		p.serviceStatus = message
	case "ServiceReady":
		p.serviceStatus = ""
	}
}

// normalizeToken converts a progress token (string or integer JSON) to a string key.
func normalizeToken(raw json.RawMessage) string {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	// Must be a number.
	return string(raw)
}
