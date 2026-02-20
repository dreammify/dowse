//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupGoWorkspace creates a temporary Go module with git init and a
// .dowse.toml mapping .go files to gopls.
func setupGoWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gows")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	// Resolve symlinks so paths match what dowse resolves internally.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "go", "mod", "init", "testworkspace")

	writeFile(t, filepath.Join(dir, ".dowse.toml"), `[[lsp]]
extensions = [".go"]
command = ["gopls"]
`)
	return dir
}

func TestGopls_SelfDiagnostics(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	instance := newDowseInstance(binary, home)

	root := projectRoot(t)
	dowseToml := filepath.Join(root, ".dowse.toml")

	// Temporarily create .dowse.toml at the project root if it doesn't exist.
	_, err := os.Stat(dowseToml)
	createdConfig := os.IsNotExist(err)
	if createdConfig {
		writeFile(t, dowseToml, `[[lsp]]
extensions = [".go"]
command = ["gopls"]
`)
		t.Cleanup(func() { os.Remove(dowseToml) })
	}

	instance.start(t)

	targetFile := filepath.Join(root, "internal", "cache", "cache.go")
	result := instance.diagnostics(t, targetFile, "--timeout", "10s")

	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean source, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestGopls_UnusedVariable(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	mainGo := filepath.Join(workspace, "main.go")
	writeFile(t, mainGo, `package main

func main() {
	x := 42
}
`)

	result := instance.diagnosticsWithRetry(t, mainGo, 15*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "10s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		if strings.Contains(diag.Message, "declared and not used") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'declared and not used' diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestGopls_FixError(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	mainGo := filepath.Join(workspace, "main.go")

	// Write file with error.
	writeFile(t, mainGo, `package main

func main() {
	x := 42
}
`)

	// Wait for the error to be detected.
	instance.diagnosticsWithRetry(t, mainGo, 15*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "10s",
	)

	// Fix the error.
	writeFile(t, mainGo, `package main

import "fmt"

func main() {
	x := 42
	fmt.Println(x)
}
`)

	// Wait for diagnostics to clear.
	result := instance.diagnosticsWithRetry(t, mainGo, 15*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount == 0 },
		"--timeout", "10s",
	)

	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors after fix, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestGopls_MultipleErrors(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	mainGo := filepath.Join(workspace, "main.go")
	writeFile(t, mainGo, `package main

func main() {
	x := 42
	y := 43
}
`)

	result := instance.diagnosticsWithRetry(t, mainGo, 15*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount >= 2 },
		"--timeout", "10s",
	)

	if result.ErrorCount != 2 {
		t.Fatalf("expected 2 errors, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestGopls_NoWaitFlag(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	mainGo := filepath.Join(workspace, "main.go")
	writeFile(t, mainGo, `package main

func main() {}
`)

	// --no-wait should return immediately with valid JSON.
	result := instance.diagnostics(t, mainGo, "--no-wait")
	// We just verify that it returned valid JSON with the correct file.
	if result.File == "" {
		t.Fatal("expected non-empty file in result")
	}
}

func TestGopls_UnknownExtension(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	txtFile := filepath.Join(workspace, "readme.txt")
	writeFile(t, txtFile, "hello world")

	cmd := exec.Command(binary, "diagnostics", txtFile)
	cmd.Env = append(os.Environ(), "DOWSE_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected error for .txt file, got success: %s", out)
	}
	if !strings.Contains(string(out), "no LSP configured for extension") {
		t.Fatalf("expected 'no LSP configured for extension' error, got: %s", out)
	}
}

func TestGopls_Definition(t *testing.T) {
	binary := buildDowse(t)
	home := setupDowseHome(t)
	workspace := setupGoWorkspace(t)
	instance := newDowseInstance(binary, home)
	instance.start(t)

	mainGo := filepath.Join(workspace, "main.go")
	writeFile(t, mainGo, `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`)

	// Warm up the session by requesting diagnostics first.
	instance.diagnosticsWithRetry(t, mainGo, 15*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount == 0 },
		"--timeout", "10s",
	)

	// Request definition on "Println" (line 6, character 6 = the 'P' in fmt.Println).
	result := instance.definitionWithRetry(t, mainGo, 6, 6, 15*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location")
	}

	loc := result.Locations[0]
	// The definition should point into the fmt package.
	if !strings.Contains(loc.File, "fmt") {
		t.Errorf("expected definition in fmt package, got file %q", loc.File)
	}
	if !strings.Contains(loc.Context, "Println") {
		t.Errorf("expected context to contain 'Println', got %q", loc.Context)
	}
}
