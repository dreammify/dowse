package cache

import (
	"context"
	"sync"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func mkDiag(msg string) Diagnostic {
	return Diagnostic{
		Range: Range{
			Start: Position{Line: 0, Character: 0},
			End:   Position{Line: 0, Character: 5},
		},
		Severity: intPtr(1),
		Message:  msg,
	}
}

func TestStoreAndRetrieve(t *testing.T) {
	c := New()
	diags := []Diagnostic{mkDiag("err1"), mkDiag("err2")}

	c.Update("file:///a.go", intPtr(1), diags)

	got, stale := c.Get("file:///a.go")
	if stale {
		t.Fatal("expected fresh, got stale")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(got))
	}
	if got[0].Message != "err1" || got[1].Message != "err2" {
		t.Fatalf("unexpected diagnostics: %v", got)
	}
}

func TestStaleDetectionVersionBased(t *testing.T) {
	c := New()
	uri := "file:///a.go"

	// Record change at version 2, update at version 1 -> stale.
	c.RecordChange(uri, 2)
	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("old")})

	_, stale := c.Get(uri)
	if !stale {
		t.Fatal("expected stale when diagnostics version < sent version")
	}

	// Update at version 2 -> fresh.
	c.Update(uri, intPtr(2), []Diagnostic{mkDiag("new")})

	got, stale := c.Get(uri)
	if stale {
		t.Fatal("expected fresh when diagnostics version >= sent version")
	}
	if got[0].Message != "new" {
		t.Fatalf("expected 'new', got %q", got[0].Message)
	}
}

func TestStaleDetectionTimestampBased(t *testing.T) {
	c := New()
	uri := "file:///b.go"

	// RecordChange with a nonzero version, then Update with nil version.
	// Freshness falls to timestamp comparison because diagnosticsVersion is nil.
	c.RecordChange(uri, 1)
	time.Sleep(5 * time.Millisecond)
	c.Update(uri, nil, []Diagnostic{mkDiag("ts-fresh")})

	got, stale := c.Get(uri)
	if stale {
		t.Fatal("expected fresh: update timestamp should be after change timestamp")
	}
	if got[0].Message != "ts-fresh" {
		t.Fatalf("expected 'ts-fresh', got %q", got[0].Message)
	}

	// RecordChange again -> stale (change timestamp now after diagnostics timestamp).
	time.Sleep(5 * time.Millisecond)
	c.RecordChange(uri, 2)

	_, stale = c.Get(uri)
	if !stale {
		t.Fatal("expected stale: change timestamp should be after diagnostics timestamp")
	}
}

func TestWaitForFreshImmediate(t *testing.T) {
	c := New()
	uri := "file:///c.go"

	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("ready")})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := c.WaitForFresh(ctx, uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Message != "ready" {
		t.Fatalf("unexpected diagnostics: %v", got)
	}
}

func TestWaitForFreshDelayed(t *testing.T) {
	c := New()
	uri := "file:///d.go"

	// Make it stale first.
	c.RecordChange(uri, 2)
	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("old")})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var got []Diagnostic
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		got, err = c.WaitForFresh(ctx, uri)
	}()

	// After 50ms, provide fresh data.
	time.Sleep(50 * time.Millisecond)
	c.Update(uri, intPtr(2), []Diagnostic{mkDiag("fresh")})

	wg.Wait()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Message != "fresh" {
		t.Fatalf("expected fresh diagnostics, got: %v", got)
	}
}

func TestWaitForFreshTimeout(t *testing.T) {
	c := New()
	uri := "file:///e.go"

	c.RecordChange(uri, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.WaitForFresh(ctx, uri)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestWaitForFreshCancellation(t *testing.T) {
	c := New()
	uri := "file:///f.go"

	c.RecordChange(uri, 1)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err = c.WaitForFresh(ctx, uri)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()

	if err != context.Canceled {
		t.Fatalf("expected Canceled, got: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New()
	uri := "file:///g.go"
	const goroutines = 20
	const iterations = 100

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Concurrent Updates.
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range iterations {
				v := i*iterations + j + 1
				c.Update(uri, intPtr(v), []Diagnostic{mkDiag("diag")})
			}
		}()
	}

	// Concurrent RecordChanges.
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range iterations {
				c.RecordChange(uri, i*iterations+j+1)
			}
		}()
	}

	// Concurrent Gets.
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				c.Get(uri)
			}
		}()
	}

	// Concurrent WaitForFresh with short timeouts.
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				waitCtx, waitCancel := context.WithTimeout(ctx, time.Millisecond)
				_, _ = c.WaitForFresh(waitCtx, uri)
				waitCancel()
			}
		}()
	}

	wg.Wait()
}

// Regression: gopls can send an empty publishDiagnostics before the daemon
// calls RecordChange. The Update that arrived before RecordChange must not
// satisfy freshness, otherwise stale (empty) diagnostics are returned.
func TestRegression_UpdateBeforeRecordChangeIsNotFresh(t *testing.T) {
	c := New()
	uri := "file:///race.go"

	// LSP sends publishDiagnostics (empty) — this arrives before RecordChange.
	c.Update(uri, intPtr(1), []Diagnostic{})

	// Now the daemon calls RecordChange.
	c.RecordChange(uri, 1)

	// Should be stale: the update predates the change.
	_, stale := c.Get(uri)
	if !stale {
		t.Fatal("expected stale: Update arrived before RecordChange")
	}

	// LSP sends real diagnostics — this arrives after RecordChange.
	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("real error")})

	got, stale := c.Get(uri)
	if stale {
		t.Fatal("expected fresh after second Update")
	}
	if len(got) != 1 || got[0].Message != "real error" {
		t.Fatalf("expected 'real error', got: %v", got)
	}
}

// Regression: WaitForFresh must not return diagnostics from an Update that
// preceded RecordChange, otherwise callers receive stale results without blocking.
func TestRegression_WaitForFreshSkipsPreExistingUpdate(t *testing.T) {
	c := New()
	uri := "file:///wait-race.go"

	// Pre-existing update.
	c.Update(uri, intPtr(1), []Diagnostic{})
	// RecordChange invalidates it.
	c.RecordChange(uri, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var got []Diagnostic
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		got, err = c.WaitForFresh(ctx, uri)
	}()

	// After a short delay, deliver the real diagnostics.
	time.Sleep(50 * time.Millisecond)
	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("real")})

	wg.Wait()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Message != "real" {
		t.Fatalf("expected 'real', got: %v", got)
	}
}

func TestRemove(t *testing.T) {
	c := New()
	uri := "file:///test.go"
	c.RecordChange(uri, 1)
	c.Update(uri, intPtr(1), []Diagnostic{mkDiag("test")})

	diags, stale := c.Get(uri)
	if len(diags) != 1 || stale {
		t.Fatalf("expected 1 fresh diagnostic before remove, got %d (stale=%v)", len(diags), stale)
	}

	c.Remove(uri)

	diags, stale = c.Get(uri)
	if diags != nil {
		t.Fatalf("expected nil diagnostics after remove, got %v", diags)
	}
	if stale {
		t.Fatal("expected not stale for removed entry")
	}
}

func TestStatsEmpty(t *testing.T) {
	c := New()
	stats := c.Stats()
	if stats.TotalFiles != 0 {
		t.Fatalf("expected 0 files, got %d", stats.TotalFiles)
	}
	if stats.TotalDiags != 0 {
		t.Fatalf("expected 0 diags, got %d", stats.TotalDiags)
	}
}

func TestStats(t *testing.T) {
	c := New()
	c.Update("file:///a.go", intPtr(1), []Diagnostic{mkDiag("err1"), mkDiag("err2")})
	c.Update("file:///b.go", intPtr(1), []Diagnostic{mkDiag("err3")})

	stats := c.Stats()
	if stats.TotalFiles != 2 {
		t.Fatalf("expected 2 files, got %d", stats.TotalFiles)
	}
	if stats.TotalDiags != 3 {
		t.Fatalf("expected 3 diags, got %d", stats.TotalDiags)
	}
}

func TestGetUnknownFile(t *testing.T) {
	c := New()
	got, stale := c.Get("file:///unknown.go")
	if got != nil {
		t.Fatalf("expected nil diagnostics for unknown file, got: %v", got)
	}
	if stale {
		t.Fatal("expected not stale for unknown file")
	}
}

func TestWaitForFreshUnknownFile(t *testing.T) {
	// A file with no entry should be considered fresh (no change recorded).
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := c.WaitForFresh(ctx, "file:///new.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil diagnostics, got: %v", got)
	}
}
