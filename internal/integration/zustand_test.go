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

// zustandSetup holds the shared daemon instance for all Zustand tests.
var (
	zustandInstance *dowseInstance
	zustandHome     string
	zustandOnce     sync.Once
	zustandSetupErr error
)

// teardownZustand stops the shared Zustand daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownZustand() {
	if zustandInstance != nil {
		cmd := exec.Command(zustandInstance.binary, "stop")
		cmd.Env = zustandInstance.env
		_ = cmd.Run()
	}
	if zustandHome != "" {
		os.RemoveAll(zustandHome)
	}
	if zustandTomlPath != "" {
		os.Remove(zustandTomlPath)
	}
}

var zustandTomlPath string

func setupZustandDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not found on PATH, skipping Zustand integration tests")
	}

	zustandOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "zustand")

		// Write .dowse.toml into the zustand project root.
		zustandTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".ts\", \".tsx\"]\ncommand = [\"typescript-language-server\", \"--stdio\"]\n"
		if err := os.WriteFile(zustandTomlPath, []byte(tomlContent), 0o644); err != nil {
			zustandSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		// Ensure node_modules are installed.
		nodeModules := filepath.Join(projectDir, "node_modules")
		if _, err := os.Stat(nodeModules); os.IsNotExist(err) {
			installCmd := exec.Command("pnpm", "install")
			installCmd.Dir = projectDir
			installOut, installErr := installCmd.CombinedOutput()
			if installErr != nil {
				zustandSetupErr = fmt.Errorf("pnpm install: %v\n%s", installErr, installOut)
				return
			}
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			zustandSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		zustandHome = dir
		zustandInstance = newDowseInstance(binary, zustandHome)

		// Start the daemon.
		cmd := exec.Command(zustandInstance.binary, "start")
		cmd.Env = zustandInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			zustandSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout.
		warmupFile := filepath.Join(projectDir, "src", "vanilla", "shallow.ts")
		warmupCmd := exec.Command(zustandInstance.binary, "diagnostics", "--timeout", "60s", warmupFile)
		warmupCmd.Env = zustandInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			zustandSetupErr = fmt.Errorf("zustand warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if zustandSetupErr != nil {
		t.Fatalf("zustand daemon setup failed: %v", zustandSetupErr)
	}
	return zustandInstance
}

func zustandProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "zustand")
}

func TestZustand_CleanFile(t *testing.T) {
	instance := setupZustandDaemon(t)
	cleanFile := filepath.Join(zustandProjectDir(t), "src", "vanilla", "shallow.ts")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestZustand_TypeMismatch(t *testing.T) {
	instance := setupZustandDaemon(t)
	targetFile := filepath.Join(zustandProjectDir(t), "src", "vanilla", "shallow.ts")

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

func TestZustand_UnresolvedReference(t *testing.T) {
	instance := setupZustandDaemon(t)
	targetFile := filepath.Join(zustandProjectDir(t), "src", "vanilla", "shallow.ts")

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

// TestZustand_Definition must run before tests that modify react.ts.
func TestZustand_Definition(t *testing.T) {
	instance := setupZustandDaemon(t)
	targetFile := filepath.Join(zustandProjectDir(t), "src", "react.ts")

	// "createStore" on line 2, character 10 (inside the import name).
	// Should resolve to vanilla.ts where createStore is defined.
	result := instance.definitionWithRetry(t, targetFile, 2, 10, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for createStore")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "vanilla.ts") {
		t.Errorf("expected definition in vanilla.ts, got %q", loc.File)
	}
	if !strings.Contains(loc.Context, "createStore") {
		t.Errorf("expected context to contain 'createStore', got %q", loc.Context)
	}
}

func TestZustand_FixError(t *testing.T) {
	instance := setupZustandDaemon(t)
	targetFile := filepath.Join(zustandProjectDir(t), "src", "vanilla", "shallow.ts")

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

func TestZustand_MultiFile(t *testing.T) {
	instance := setupZustandDaemon(t)
	shallowFile := filepath.Join(zustandProjectDir(t), "src", "vanilla", "shallow.ts")
	vanillaFile := filepath.Join(zustandProjectDir(t), "src", "vanilla.ts")

	shallowOriginal, err := os.ReadFile(shallowFile)
	if err != nil {
		t.Fatalf("reading %s: %v", shallowFile, err)
	}
	vanillaOriginal, err := os.ReadFile(vanillaFile)
	if err != nil {
		t.Fatalf("reading %s: %v", vanillaFile, err)
	}

	// Inject errors in both files.
	shallowBroken := string(shallowOriginal) + "\nconst shallowBroken: number = \"hello\";\n"
	vanillaBroken := string(vanillaOriginal) + "\nconst vanillaBroken: number = \"hello\";\n"

	if err := os.WriteFile(shallowFile, []byte(shallowBroken), 0o644); err != nil {
		t.Fatalf("writing broken shallow file: %v", err)
	}
	if err := os.WriteFile(vanillaFile, []byte(vanillaBroken), 0o644); err != nil {
		t.Fatalf("writing broken vanilla file: %v", err)
	}
	t.Cleanup(func() {
		os.WriteFile(shallowFile, shallowOriginal, 0o644)
		os.WriteFile(vanillaFile, vanillaOriginal, 0o644)
	})

	// Verify both files have errors.
	instance.diagnosticsWithRetry(t, shallowFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)
	instance.diagnosticsWithRetry(t, vanillaFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)
}
