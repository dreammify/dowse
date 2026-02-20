package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a temp directory with a git repo and an initial commit on "main".
// Returns the symlink-resolved path (important on macOS where /var -> /private/var).
func initGitRepo(t *testing.T) string {
	t.Helper()
	rawDir := t.TempDir()
	dir, err := filepath.EvalSymlinks(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "base.go"), []byte("package base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "base.go")
	run("commit", "-m", "initial")
	return dir
}

// chdirTo changes the working directory and registers a cleanup to restore it.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origDir); err != nil {
			t.Logf("restoring working directory: %v", err)
		}
	})
}

func TestGitDiffFilesCommittedBranchChanges(t *testing.T) {
	dir := initGitRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create a feature branch and commit a change.
	run("checkout", "-b", "feature")
	featureFile := filepath.Join(dir, "feature.go")
	if err := os.WriteFile(featureFile, []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "feature.go")
	run("commit", "-m", "add feature")

	chdirTo(t, dir)

	files, err := gitDiffFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files[0] != featureFile {
		t.Errorf("expected %s, got %s", featureFile, files[0])
	}
}

func TestGitDiffFilesUncommittedChanges(t *testing.T) {
	dir := initGitRepo(t)
	chdirTo(t, dir)

	// Modify an existing file without committing.
	if err := os.WriteFile(filepath.Join(dir, "base.go"), []byte("package base\n// modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := gitDiffFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "base.go" {
		t.Errorf("expected base.go, got %s", files[0])
	}
}

func TestGitDiffFilesUntrackedFiles(t *testing.T) {
	dir := initGitRepo(t)
	chdirTo(t, dir)

	// Create a new untracked file.
	untrackedFile := filepath.Join(dir, "new.go")
	if err := os.WriteFile(untrackedFile, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := gitDiffFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files[0] != untrackedFile {
		t.Errorf("expected %s, got %s", untrackedFile, files[0])
	}
}

func TestGitDiffFilesNoDuplicates(t *testing.T) {
	dir := initGitRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	chdirTo(t, dir)

	// Create branch, commit a file, then modify it again (uncommitted).
	// The file should appear once, not twice.
	run("checkout", "-b", "feature")
	featureFile := filepath.Join(dir, "feature.go")
	if err := os.WriteFile(featureFile, []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "feature.go")
	run("commit", "-m", "add feature")

	// Now modify the same file without committing.
	if err := os.WriteFile(featureFile, []byte("package feature\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := gitDiffFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file (no duplicates), got %d: %v", len(files), files)
	}
}

func TestGitDiffFilesNoChanges(t *testing.T) {
	dir := initGitRepo(t)
	chdirTo(t, dir)

	// No changes at all — should return empty.
	files, err := gitDiffFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d: %v", len(files), files)
	}
}

func TestDiffBaseFallsBackToHEAD(t *testing.T) {
	dir := initGitRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	chdirTo(t, dir)

	// Rename main so neither "main" nor "master" exists.
	run("branch", "-m", "main", "trunk")

	base, err := diffBase(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if base != "HEAD" {
		t.Errorf("expected HEAD fallback, got %s", base)
	}
}
