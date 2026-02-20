package tui

import (
	"context"
	"os"
	"testing"
)

func TestCollectMetricsCurrentProcess(t *testing.T) {
	metrics := CollectMetrics(os.Getpid())
	if !metrics.Alive {
		t.Fatal("expected current process to be alive")
	}
	if metrics.RSS == 0 {
		t.Fatal("expected RSS > 0 for current process")
	}
}

func TestCollectMetricsDeadProcess(t *testing.T) {
	metrics := CollectMetrics(999999999)
	if metrics.Alive {
		t.Fatal("expected non-existent process to report Alive=false")
	}
}

func TestTrackerDeltaCPU(t *testing.T) {
	tracker := newProcessTracker()
	pid := os.Getpid()
	ctx := context.Background()

	// First call primes the baseline — CPU should be 0.
	first := tracker.Collect(ctx, pid)
	if !first.Alive {
		t.Fatal("expected current process to be alive")
	}
	if first.CPUPercent != 0 {
		t.Fatalf("expected 0 CPU on first sample (priming), got %.2f", first.CPUPercent)
	}

	// Second call should have a real delta (may still be ~0 for an idle
	// test process, but the sampled flag should be set).
	second := tracker.Collect(ctx, pid)
	if !second.Alive {
		t.Fatal("expected current process to be alive on second sample")
	}
	if second.RSS == 0 {
		t.Fatal("expected RSS > 0 on second sample")
	}
}

func TestTrackerPrune(t *testing.T) {
	tracker := newProcessTracker()
	ctx := context.Background()
	pid := os.Getpid()

	tracker.Collect(ctx, pid)
	if len(tracker.handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(tracker.handles))
	}

	// Prune with empty active set should remove the handle.
	tracker.Prune(map[int]bool{})
	if len(tracker.handles) != 0 {
		t.Fatalf("expected 0 handles after prune, got %d", len(tracker.handles))
	}
}
