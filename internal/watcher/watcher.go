// Package watcher provides a filesystem watcher that monitors workspace directories
// for file changes, filters by extension, respects .gitignore, and debounces events.
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounceDelay = 50 * time.Millisecond
const ignoreCacheTTL = 30 * time.Second
const ignoreCacheMaxSize = 10000

// EventKind classifies a file system event.
type EventKind int

const (
	EventCreated EventKind = iota
	EventModified
	EventDeleted
)

func (k EventKind) String() string {
	switch k {
	case EventCreated:
		return "CREATED"
	case EventModified:
		return "MODIFIED"
	case EventDeleted:
		return "DELETED"
	default:
		return "UNKNOWN"
	}
}

type ignoreCacheEntry struct {
	ignored   bool
	expiresAt time.Time
}

// Event represents a filtered, debounced file system event.
type Event struct {
	Path string
	Kind EventKind
}

// Watcher watches a directory tree for file changes filtered by extension,
// respecting .gitignore. Events are debounced per-file.
type Watcher struct {
	root       string
	extensions map[string]struct{}
	fsw        *fsnotify.Watcher

	ignoreMu    sync.RWMutex
	ignoreCache map[string]ignoreCacheEntry
}

// New creates a Watcher for the given root directory. Only files whose
// extension appears in extensions (e.g. ".go", ".kt") produce events.
func New(root string, extensions []string) (*Watcher, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	extSet := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		extSet[ext] = struct{}{}
	}

	w := &Watcher{
		root:        abs,
		extensions:  extSet,
		fsw:         fsw,
		ignoreCache: make(map[string]ignoreCacheEntry),
	}

	if err := w.addDirs(abs); err != nil {
		fsw.Close()
		return nil, err
	}

	return w, nil
}

// addDirs recursively walks from dir and adds every directory to the
// fsnotify watcher, skipping hidden directories (starting with ".").
func (w *Watcher) addDirs(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base != "." && strings.HasPrefix(base, ".") {
				return fs.SkipDir
			}
			return w.fsw.Add(path)
		}
		return nil
	})
}

// Watch starts the event loop. It returns a channel on which filtered,
// debounced events are sent. Watch blocks until ctx is cancelled, at which
// point the channel is closed and the underlying fsnotify watcher is shut down.
func (w *Watcher) Watch(ctx context.Context) (<-chan Event, error) {
	filtered, _, err := w.WatchAll(ctx)
	return filtered, err
}

// WatchAll starts the event loop and returns two channels:
//   - filtered: events for files matching the configured extensions (same as Watch)
//   - all: every non-directory, non-gitignored file event regardless of extension
//
// Both channels share the same debounce infrastructure. The all channel enables
// workspace/didChangeWatchedFiles notifications for build files (e.g., gradle).
func (w *Watcher) WatchAll(ctx context.Context) (filtered <-chan Event, all <-chan Event, err error) {
	filteredCh := make(chan Event, 64)
	allCh := make(chan Event, 64)

	go w.loopAll(ctx, filteredCh, allCh)

	return filteredCh, allCh, nil
}

// pendingEvent tracks debounce state for a single path.
type pendingEvent struct {
	timer    *time.Timer
	kind     EventKind
	filtered bool // whether this path matches extension filters
}

// loopState holds shared mutable state for the event loop, protected by mu.
type loopState struct {
	mu      sync.Mutex
	pending map[string]*pendingEvent
	closed  bool // set to true when the loop exits; prevents timer sends on closed channels
}

func (w *Watcher) loopAll(ctx context.Context, filtered chan<- Event, all chan<- Event) {
	state := &loopState{
		pending: make(map[string]*pendingEvent),
	}

	defer func() {
		state.mu.Lock()
		state.closed = true
		for _, pendingEv := range state.pending {
			pendingEv.timer.Stop()
		}
		// Close channels under the lock to prevent timer goroutines
		// from sending on closed channels. Timer goroutines check
		// state.closed under the same lock before sending.
		close(filtered)
		close(all)
		state.mu.Unlock()
		w.fsw.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case fsEvent, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEventAll(ctx, fsEvent, filtered, all, state)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Error("fsnotify error", "err", err)
		}
	}
}

func (w *Watcher) handleEventAll(
	ctx context.Context,
	fsEvent fsnotify.Event,
	filtered chan<- Event,
	all chan<- Event,
	state *loopState,
) {
	path := fsEvent.Name

	// If a directory was created, add it (and any nested dirs) to the watch set.
	if fsEvent.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if err := w.addDirs(path); err != nil {
				slog.Error("failed to watch new directory", "path", path, "err", err)
			}
			return // directories themselves don't produce events
		}
	}

	// .gitignore filter (applies to both channels).
	if w.isGitIgnored(ctx, path) {
		return
	}

	// Extension filter determines which channels receive the event.
	ext := filepath.Ext(path)
	_, matchesExt := w.extensions[ext]

	kind := classifyEvent(fsEvent)

	// Debounce: reset the timer for this path. A Create followed by Write
	// within the debounce window is still reported as Created.
	state.mu.Lock()
	if existing, ok := state.pending[path]; ok {
		existing.timer.Stop()
		// Preserve Create if a Write follows within the window.
		if existing.kind == EventCreated && kind == EventModified {
			kind = EventCreated
		}
	}
	pendingEntry := &pendingEvent{kind: kind, filtered: matchesExt}
	pendingEntry.timer = time.AfterFunc(debounceDelay, func() {
		state.mu.Lock()
		if state.closed {
			state.mu.Unlock()
			return
		}
		entryKind := pendingEntry.kind
		isFiltered := pendingEntry.filtered
		delete(state.pending, path)

		// Send on channels while holding the lock. This is safe because
		// the channels are buffered (64 items) and the defer that closes
		// them also holds this lock. This prevents a race between send
		// and close.
		event := Event{Path: path, Kind: entryKind}
		if isFiltered {
			select {
			case filtered <- event:
			default:
				// Buffer full — drop event rather than deadlock.
			}
		}
		select {
		case all <- event:
		default:
		}
		state.mu.Unlock()
	})
	state.pending[path] = pendingEntry
	state.mu.Unlock()
}

func classifyEvent(fsEvent fsnotify.Event) EventKind {
	switch {
	case fsEvent.Has(fsnotify.Create):
		return EventCreated
	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		return EventDeleted
	default:
		return EventModified
	}
}

// isGitIgnored checks whether path is git-ignored, caching results to avoid
// spawning a subprocess on every file event. Cache entries expire after
// ignoreCacheTTL so that .gitignore changes are eventually picked up.
func (w *Watcher) isGitIgnored(ctx context.Context, path string) bool {
	now := time.Now()

	w.ignoreMu.RLock()
	entry, ok := w.ignoreCache[path]
	w.ignoreMu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.ignored
	}

	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", path)
	cmd.Dir = w.root
	err := cmd.Run()
	// Exit 0 means the file IS ignored; exit 1 means it is NOT ignored.
	ignored := err == nil

	w.ignoreMu.Lock()
	w.ignoreCache[path] = ignoreCacheEntry{ignored: ignored, expiresAt: now.Add(ignoreCacheTTL)}
	if len(w.ignoreCache) > ignoreCacheMaxSize {
		w.sweepExpiredLocked(now)
	}
	w.ignoreMu.Unlock()

	return ignored
}

// sweepExpiredLocked removes all expired entries from the ignoreCache.
// Must be called with ignoreMu held.
func (w *Watcher) sweepExpiredLocked(now time.Time) {
	for path, entry := range w.ignoreCache {
		if now.After(entry.expiresAt) {
			delete(w.ignoreCache, path)
		}
	}
}
