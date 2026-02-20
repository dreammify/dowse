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
	out := make(chan Event, 64)

	go w.loop(ctx, out)

	return out, nil
}

// pendingEvent tracks debounce state for a single path.
type pendingEvent struct {
	timer *time.Timer
	kind  EventKind
}

func (w *Watcher) loop(ctx context.Context, out chan<- Event) {
	var mu sync.Mutex
	pending := make(map[string]*pendingEvent)

	defer func() {
		mu.Lock()
		for _, pendingEv := range pending {
			pendingEv.timer.Stop()
		}
		mu.Unlock()
		w.fsw.Close()
		close(out)
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case fsEvent, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ctx, fsEvent, out, &mu, pending)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Error("fsnotify error", "err", err)
		}
	}
}

func (w *Watcher) handleEvent(
	ctx context.Context,
	fsEvent fsnotify.Event,
	out chan<- Event,
	mu *sync.Mutex,
	pending map[string]*pendingEvent,
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

	// Extension filter.
	ext := filepath.Ext(path)
	if _, ok := w.extensions[ext]; !ok {
		return
	}

	// .gitignore filter.
	if w.isGitIgnored(ctx, path) {
		return
	}

	kind := classifyEvent(fsEvent)

	// Debounce: reset the timer for this path. A Create followed by Write
	// within the debounce window is still reported as Created.
	mu.Lock()
	if existing, ok := pending[path]; ok {
		existing.timer.Stop()
		// Preserve Create if a Write follows within the window.
		if existing.kind == EventCreated && kind == EventModified {
			kind = EventCreated
		}
	}
	pendingEntry := &pendingEvent{kind: kind}
	pendingEntry.timer = time.AfterFunc(debounceDelay, func() {
		mu.Lock()
		kind := pendingEntry.kind
		delete(pending, path)
		mu.Unlock()

		select {
		case <-ctx.Done():
		case out <- Event{Path: path, Kind: kind}:
		}
	})
	pending[path] = pendingEntry
	mu.Unlock()
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
