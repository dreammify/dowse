// Command testwatcher is a standalone debugging tool for the watcher package.
// It watches a directory tree for file changes, filtered by extension and
// respecting .gitignore, and prints each debounced event to stdout.
//
// Usage:
//
//	go run ./cmd/testwatcher -root . -extensions ".go,.kt"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/dreammify/dowse/internal/watcher"
)

func main() {
	root := flag.String("root", ".", "workspace root directory")
	exts := flag.String("extensions", ".go", "comma-separated file extensions to watch")
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	extensions := strings.Split(*exts, ",")
	for i, ext := range extensions {
		ext = strings.TrimSpace(ext)
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		extensions[i] = ext
	}

	w, err := watcher.New(absRoot, extensions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ch, err := w.Watch(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "watching %s for %s\n", absRoot, strings.Join(extensions, ", "))

	for ev := range ch {
		rel, err := filepath.Rel(absRoot, ev.Path)
		if err != nil {
			rel = ev.Path
		}
		fmt.Printf("%-9s %s\n", ev.Kind, rel)
	}
}
