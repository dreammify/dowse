package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// initGitRepo initialises a git repo in dir so that git check-ignore works.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// recv reads one event from ch with a generous timeout.
func recv(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return Event{} // unreachable
	}
}

// expectNoEvent asserts that no event arrives within the given duration.
func expectNoEvent(t *testing.T, ch <-chan Event, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(d):
		// good
	}
}

// drain collects all events that arrive within the given duration.
func drain(ch <-chan Event, d time.Duration) []Event {
	var out []Event
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func TestBasicEvents(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Allow the watcher to settle.
	time.Sleep(100 * time.Millisecond)

	// Create
	fpath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(fpath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, ch, 3*time.Second)
	if ev.Kind != EventCreated {
		t.Fatalf("expected EventCreated, got %v", ev.Kind)
	}
	if ev.Path != fpath {
		t.Fatalf("expected path %s, got %s", fpath, ev.Path)
	}

	// Wait for debounce to fully settle before next operation.
	time.Sleep(200 * time.Millisecond)

	// Modify
	if err := os.WriteFile(fpath, []byte("package main\nfunc main(){}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ev = recv(t, ch, 3*time.Second)
	if ev.Kind != EventModified {
		t.Fatalf("expected EventModified, got %v", ev.Kind)
	}

	// Wait for debounce to fully settle.
	time.Sleep(200 * time.Millisecond)

	// Delete
	if err := os.Remove(fpath); err != nil {
		t.Fatal(err)
	}
	ev = recv(t, ch, 3*time.Second)
	if ev.Kind != EventDeleted {
		t.Fatalf("expected EventDeleted, got %v", ev.Kind)
	}
}

func TestExtensionFiltering(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create a .txt file -- should be ignored.
	txtPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txtPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a .go file -- should be emitted.
	goPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch, 3*time.Second)
	if ev.Path != goPath {
		t.Fatalf("expected event for %s, got %s", goPath, ev.Path)
	}
	if ev.Kind != EventCreated {
		t.Fatalf("expected EventCreated, got %v", ev.Kind)
	}

	// No more events should arrive for the .txt file.
	expectNoEvent(t, ch, 300*time.Millisecond)
}

func TestGitignoreFiltering(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Add *.log to .gitignore and commit it so git knows about it.
	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Watch both .log and .go so extension filter doesn't mask gitignore.
	w, err := New(dir, []string{".log", ".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create debug.log -- git-ignored, should produce no event.
	logPath := filepath.Join(dir, "debug.log")
	if err := os.WriteFile(logPath, []byte("log entry"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a .go file to confirm the watcher is working.
	goPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch, 3*time.Second)
	if ev.Path != goPath {
		t.Fatalf("expected event for %s, got event for %s (kind=%v)", goPath, ev.Path, ev.Kind)
	}

	expectNoEvent(t, ch, 300*time.Millisecond)
}

func TestGitignoreCaching(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	w, err := New(dir, []string{".log", ".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	logPath := filepath.Join(dir, "debug.log")

	// Write the gitignored file multiple times. Each event should be
	// filtered. The first write populates the cache; subsequent writes
	// hit it instead of spawning a subprocess.
	for range 3 {
		if err := os.WriteFile(logPath, []byte("entry"), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Confirm the cache is populated for the ignored path.
	w.ignoreMu.RLock()
	entry, ok := w.ignoreCache[logPath]
	w.ignoreMu.RUnlock()
	if !ok {
		t.Fatal("expected gitignore cache entry for ignored file")
	}
	if !entry.ignored {
		t.Fatal("expected cached value to be ignored")
	}
	if entry.expiresAt.IsZero() {
		t.Fatal("expected non-zero TTL expiry")
	}

	// Write a non-ignored .go file to confirm watcher still works.
	goPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch, 3*time.Second)
	if ev.Path != goPath {
		t.Fatalf("expected event for %s, got %s", goPath, ev.Path)
	}

	// Verify non-ignored file is also cached.
	w.ignoreMu.RLock()
	entry, ok = w.ignoreCache[goPath]
	w.ignoreMu.RUnlock()
	if !ok {
		t.Fatal("expected gitignore cache entry for non-ignored file")
	}
	if entry.ignored {
		t.Fatal("expected cached value to be not ignored")
	}

	expectNoEvent(t, ch, 300*time.Millisecond)
}

func TestNestedDirectoryCreation(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create a nested directory.
	sub := filepath.Join(dir, "pkg", "util")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	// Give the watcher time to pick up the new directories.
	time.Sleep(300 * time.Millisecond)

	// Create a file in the nested directory.
	fpath := filepath.Join(sub, "helper.go")
	if err := os.WriteFile(fpath, []byte("package util\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch, 3*time.Second)
	if ev.Path != fpath {
		t.Fatalf("expected event for %s, got %s", fpath, ev.Path)
	}
	if ev.Kind != EventCreated {
		t.Fatalf("expected EventCreated, got %v", ev.Kind)
	}
}

func TestDebouncing(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create the file first, consume the Created event.
	fpath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(fpath, []byte("v0"), 0644); err != nil {
		t.Fatal(err)
	}
	recv(t, ch, 3*time.Second) // consume Created

	// Wait for everything to settle.
	time.Sleep(200 * time.Millisecond)

	// Write 10 times in rapid succession.
	const writes = 10
	for i := 0; i < writes; i++ {
		if err := os.WriteFile(fpath, []byte("v"+string(rune('1'+i))), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Collect events over a generous window that covers the debounce period.
	// On macOS, fsnotify may deliver events with irregular timing that splits
	// them across debounce windows, so we verify coalescing happened (fewer
	// events than writes) rather than requiring exactly 1.
	events := drain(ch, 500*time.Millisecond)
	if len(events) == 0 {
		t.Fatal("expected at least 1 debounced event, got 0")
	}
	if len(events) >= writes {
		t.Fatalf("expected debouncing to coalesce %d writes, but got %d events: %+v", writes, len(events), events)
	}
}

// Regression: on Linux (inotify), os.WriteFile for a new file produces
// Create then Write events. The debounce window must preserve the Create
// kind when a Write follows, rather than overwriting it with Modified.
func TestRegression_CreateNotLostToDebouncedWrite(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	fpath := filepath.Join(dir, "new.go")

	// Simulate what inotify does on Linux: a Create event followed
	// immediately by a Write event for the same file.
	if err := os.WriteFile(fpath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch, 3*time.Second)
	if ev.Path != fpath {
		t.Fatalf("expected event for %s, got %s", fpath, ev.Path)
	}
	if ev.Kind != EventCreated {
		t.Fatalf("expected EventCreated, got %v (Create was lost to debounced Write)", ev.Kind)
	}
}

func TestWatchAll(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Only watch .go files for the filtered channel.
	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	filtered, all, err := w.WatchAll(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create a .gradle.kts file — should appear on `all` but NOT on `filtered`.
	gradlePath := filepath.Join(dir, "build.gradle.kts")
	if err := os.WriteFile(gradlePath, []byte("plugins {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for the event on the `all` channel.
	allEvent := recv(t, all, 3*time.Second)
	if allEvent.Path != gradlePath {
		t.Fatalf("expected all-channel event for %s, got %s", gradlePath, allEvent.Path)
	}
	if allEvent.Kind != EventCreated {
		t.Fatalf("expected EventCreated on all-channel, got %v", allEvent.Kind)
	}

	// The filtered channel should NOT have received this event.
	expectNoEvent(t, filtered, 300*time.Millisecond)

	// Now create a .go file — should appear on BOTH channels.
	goPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	filteredEvent := recv(t, filtered, 3*time.Second)
	if filteredEvent.Path != goPath {
		t.Fatalf("expected filtered event for %s, got %s", goPath, filteredEvent.Path)
	}

	allEvent2 := recv(t, all, 3*time.Second)
	if allEvent2.Path != goPath {
		t.Fatalf("expected all-channel event for %s, got %s", goPath, allEvent2.Path)
	}
}

func TestIgnoreCacheEviction(t *testing.T) {
	w := &Watcher{
		ignoreCache: make(map[string]ignoreCacheEntry),
	}

	now := time.Now()

	// Fill the cache past the limit with expired entries.
	for i := 0; i < ignoreCacheMaxSize+100; i++ {
		path := filepath.Join("/fake", "path", string(rune(i/256+'a')), string(rune(i%256+'a')))
		w.ignoreCache[path] = ignoreCacheEntry{
			ignored:   false,
			expiresAt: now.Add(-1 * time.Second), // expired
		}
	}

	if len(w.ignoreCache) <= ignoreCacheMaxSize {
		t.Fatalf("setup: expected cache size > %d, got %d", ignoreCacheMaxSize, len(w.ignoreCache))
	}

	w.ignoreMu.Lock()
	w.sweepExpiredLocked(now)
	w.ignoreMu.Unlock()

	if len(w.ignoreCache) != 0 {
		t.Errorf("expected all expired entries swept, got %d remaining", len(w.ignoreCache))
	}
}

func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	w, err := New(dir, []string{".go"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	cancel()

	// The channel should close.
	select {
	case _, ok := <-ch:
		if ok {
			// It's possible to get a lingering event; drain until closed.
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event channel was not closed after context cancellation")
	}
}
