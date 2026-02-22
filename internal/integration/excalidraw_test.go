//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// excalidrawSetup holds the shared daemon instance for all Excalidraw tests.
// TypeScript LSP initialization plus yarn install can be slow on cold start.
var (
	excalidrawInstance *dowseInstance
	excalidrawHome     string
	excalidrawOnce     sync.Once
	excalidrawSetupErr error
)

// teardownExcalidraw stops the shared Excalidraw daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownExcalidraw() {
	if excalidrawInstance != nil {
		cmd := exec.Command(excalidrawInstance.binary, "stop")
		cmd.Env = excalidrawInstance.env
		_ = cmd.Run()
	}
	if excalidrawHome != "" {
		os.RemoveAll(excalidrawHome)
	}
	// Clean up .dowse.toml written into the submodule.
	if excalidrawTomlPath != "" {
		os.Remove(excalidrawTomlPath)
	}
}

var excalidrawTomlPath string

func setupExcalidrawDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not found on PATH, skipping Excalidraw integration tests")
	}

	excalidrawOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "excalidraw")

		// Write .dowse.toml into the excalidraw project root.
		excalidrawTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".ts\", \".tsx\"]\ncommand = [\"typescript-language-server\", \"--stdio\"]\n"
		if err := os.WriteFile(excalidrawTomlPath, []byte(tomlContent), 0o644); err != nil {
			excalidrawSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		// Ensure node_modules are installed.
		nodeModules := filepath.Join(projectDir, "node_modules")
		if _, err := os.Stat(nodeModules); os.IsNotExist(err) {
			installCmd := exec.Command("yarn", "install")
			installCmd.Dir = projectDir
			installOut, installErr := installCmd.CombinedOutput()
			if installErr != nil {
				excalidrawSetupErr = fmt.Errorf("yarn install: %v\n%s", installErr, installOut)
				return
			}
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			excalidrawSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		excalidrawHome = dir
		excalidrawInstance = newDowseInstance(binary, excalidrawHome)

		// Start the daemon.
		cmd := exec.Command(excalidrawInstance.binary, "start")
		cmd.Env = excalidrawInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			excalidrawSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout.
		warmupFile := filepath.Join(projectDir, "packages", "math", "src", "vector.ts")
		warmupCmd := exec.Command(excalidrawInstance.binary, "diagnostics", "--timeout", "60s", warmupFile)
		warmupCmd.Env = excalidrawInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			excalidrawSetupErr = fmt.Errorf("excalidraw warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if excalidrawSetupErr != nil {
		t.Fatalf("excalidraw daemon setup failed: %v", excalidrawSetupErr)
	}
	return excalidrawInstance
}

func excalidrawProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "excalidraw")
}

func TestExcalidraw_CleanFile(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	cleanFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "vector.ts")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestExcalidraw_TypeMismatch(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	targetFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "vector.ts")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	brokenContent := string(originalContent) + "\nconst x: number = \"hello\";\n"
	withModifiedFile(t, targetFile, brokenContent)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "not assignable") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'not assignable' diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestExcalidraw_UnresolvedReference(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	targetFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "vector.ts")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	brokenContent := string(originalContent) + "\nconst broken = NonExistentThing;\n"
	withModifiedFile(t, targetFile, brokenContent)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "cannot find name") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'cannot find name' diagnostic, got: %+v", result.Diagnostics)
	}
}

// TestExcalidraw_Definition must run before tests that modify segment.ts.
func TestExcalidraw_Definition(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	targetFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "segment.ts")

	// "vectorFromPoint" on line 12, character 3 (inside the import name).
	// Should resolve to vector.ts where the function is defined.
	result := instance.definitionWithRetry(t, targetFile, 12, 3, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for vectorFromPoint")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "vector.ts") {
		t.Errorf("expected definition in vector.ts, got %q", loc.File)
	}
	if !strings.Contains(loc.Context, "vectorFromPoint") {
		t.Errorf("expected context to contain 'vectorFromPoint', got %q", loc.Context)
	}
}

func TestExcalidraw_CrossPackageDefinition(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	targetFile := filepath.Join(excalidrawProjectDir(t), "packages", "common", "src", "colors.ts")

	// "clamp" on line 3, character 10 (inside the import name from @excalidraw/math).
	// Should resolve to the math package.
	result := instance.definitionWithRetry(t, targetFile, 3, 10, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for clamp")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "math") {
		t.Errorf("expected definition in math package, got %q", loc.File)
	}
}

func TestExcalidraw_FixError(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	targetFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "vector.ts")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	brokenContent := string(originalContent) + "\nconst broken = NonExistentThing;\n"
	if err := os.WriteFile(targetFile, []byte(brokenContent), 0o644); err != nil {
		t.Fatalf("writing broken file: %v", err)
	}
	t.Cleanup(func() {
		os.WriteFile(targetFile, originalContent, 0o644)
	})

	// Wait for error to be detected.
	instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	// Fix the error by restoring original content.
	if err := os.WriteFile(targetFile, originalContent, 0o644); err != nil {
		t.Fatalf("restoring file: %v", err)
	}

	// Wait for diagnostics to clear.
	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount == 0 },
		"--timeout", "30s",
	)

	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors after fix, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestExcalidraw_MultiFile(t *testing.T) {
	instance := setupExcalidrawDaemon(t)
	vectorFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "vector.ts")
	pointFile := filepath.Join(excalidrawProjectDir(t), "packages", "math", "src", "point.ts")

	// Read originals for cleanup.
	vectorOriginal, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatalf("reading %s: %v", vectorFile, err)
	}
	pointOriginal, err := os.ReadFile(pointFile)
	if err != nil {
		t.Fatalf("reading %s: %v", pointFile, err)
	}

	// Inject errors in both files.
	vectorBroken := string(vectorOriginal) + "\nconst vectorBroken: number = \"hello\";\n"
	pointBroken := string(pointOriginal) + "\nconst pointBroken: number = \"hello\";\n"

	if err := os.WriteFile(vectorFile, []byte(vectorBroken), 0o644); err != nil {
		t.Fatalf("writing broken vector file: %v", err)
	}
	if err := os.WriteFile(pointFile, []byte(pointBroken), 0o644); err != nil {
		t.Fatalf("writing broken point file: %v", err)
	}
	t.Cleanup(func() {
		os.WriteFile(vectorFile, vectorOriginal, 0o644)
		os.WriteFile(pointFile, pointOriginal, 0o644)
	})

	// Verify both files have errors.
	instance.diagnosticsWithRetry(t, vectorFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)
	instance.diagnosticsWithRetry(t, pointFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)
}
