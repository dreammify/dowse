package tui

import (
	"context"
	"sync"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcessMetrics holds resource usage data for a single process.
type ProcessMetrics struct {
	PID        int
	RSS        uint64
	CPUPercent float64
	Alive      bool
}

// processTracker caches gopsutil process handles across poll cycles so that
// Percent(0) can compute CPU deltas between consecutive calls.
type processTracker struct {
	mu      sync.Mutex
	handles map[int]*trackedProcess
}

type trackedProcess struct {
	proc    *process.Process
	sampled bool // true after the first Percent(0) call (priming sample)
}

func newProcessTracker() *processTracker {
	return &processTracker{handles: make(map[int]*trackedProcess)}
}

// Collect gathers CPU and memory stats for the given PID. On the first call
// for a new PID it only collects RSS — the CPU reading requires two samples
// so the first Percent(0) call just primes the internal state.
func (t *processTracker) Collect(ctx context.Context, pid int) ProcessMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()

	metrics := ProcessMetrics{PID: pid}

	tracked, exists := t.handles[pid]
	if !exists {
		proc, err := process.NewProcessWithContext(ctx, int32(pid))
		if err != nil {
			return metrics
		}
		tracked = &trackedProcess{proc: proc}
		t.handles[pid] = tracked
	}

	memInfo, err := tracked.proc.MemoryInfoWithContext(ctx)
	if err != nil {
		// Process may have died — remove from cache.
		delete(t.handles, pid)
		return metrics
	}
	if memInfo != nil {
		metrics.RSS = memInfo.RSS
	}
	metrics.Alive = true

	// Percent(0) computes delta CPU since the last call. The first call
	// for a new process just stores the baseline — skip showing CPU to
	// avoid a garbage value from a near-zero time denominator.
	cpuPercent, err := tracked.proc.PercentWithContext(ctx, 0)
	if err != nil {
		return metrics
	}
	if tracked.sampled {
		metrics.CPUPercent = cpuPercent
	}
	tracked.sampled = true

	return metrics
}

// Prune removes tracked handles for PIDs not in the active set.
func (t *processTracker) Prune(activePIDs map[int]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid := range t.handles {
		if !activePIDs[pid] {
			delete(t.handles, pid)
		}
	}
}

// CollectMetrics is a convenience for one-off metrics collection (e.g. tests).
// It does not benefit from delta-based CPU tracking.
func CollectMetrics(pid int) ProcessMetrics {
	pt := newProcessTracker()
	return pt.Collect(context.Background(), pid)
}
