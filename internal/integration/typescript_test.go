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

// typescriptSetup holds the shared daemon instance for all TypeScript tests.
// The TypeScript LSP takes ~3-10s for cold start, plus bun install overhead.
var (
	typescriptInstance *dowseInstance
	typescriptHome     string
	typescriptOnce     sync.Once
	typescriptSetupErr error
)

// teardownTypeScript stops the shared TypeScript daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownTypeScript() {
	if typescriptInstance != nil {
		cmd := exec.Command(typescriptInstance.binary, "stop")
		cmd.Env = typescriptInstance.env
		_ = cmd.Run()
	}
	if typescriptHome != "" {
		os.RemoveAll(typescriptHome)
	}
}

func setupTypeScriptDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	// Skip if typescript-language-server is not on PATH.
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not found on PATH, skipping TypeScript integration tests")
	}

	typescriptOnce.Do(func() {
		binary := buildDowse(t)

		// Ensure node_modules are installed in the sample project.
		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "typescript-nextjs")
		nodeModules := filepath.Join(projectDir, "node_modules")
		if _, err := os.Stat(nodeModules); os.IsNotExist(err) {
			installCmd := exec.Command("bun", "install")
			installCmd.Dir = projectDir
			installOut, installErr := installCmd.CombinedOutput()
			if installErr != nil {
				typescriptSetupErr = fmt.Errorf("bun install: %v\n%s", installErr, installOut)
				return
			}
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			typescriptSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		typescriptHome = dir
		typescriptInstance = newDowseInstance(binary, typescriptHome)

		// Start the daemon.
		cmd := exec.Command(typescriptInstance.binary, "start")
		cmd.Env = typescriptInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			typescriptSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// TypeScript LSP initialization.
		warmupFile := filepath.Join(projectDir, "src", "lib", "utils.ts")
		warmupCmd := exec.Command(typescriptInstance.binary, "diagnostics", "--timeout", "60s", warmupFile)
		warmupCmd.Env = typescriptInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			typescriptSetupErr = fmt.Errorf("typescript warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if typescriptSetupErr != nil {
		t.Fatalf("typescript daemon setup failed: %v", typescriptSetupErr)
	}
	return typescriptInstance
}

func typescriptProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "typescript-nextjs")
}

func TestTypeScript_CleanFile(t *testing.T) {
	instance := setupTypeScriptDaemon(t)
	cleanFile := filepath.Join(typescriptProjectDir(t), "src", "lib", "utils.ts")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

func TestTypeScript_TypeMismatch(t *testing.T) {
	instance := setupTypeScriptDaemon(t)
	targetFile := filepath.Join(typescriptProjectDir(t), "src", "lib", "utils.ts")

	withModifiedFile(t, targetFile, `export function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

const x: number = "hello";

export function capitalize(str: string): string {
  if (str.length === 0) return str;
  return str.charAt(0).toUpperCase() + str.slice(1);
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
`)

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

func TestTypeScript_UnresolvedReference(t *testing.T) {
	instance := setupTypeScriptDaemon(t)
	targetFile := filepath.Join(typescriptProjectDir(t), "src", "lib", "utils.ts")

	withModifiedFile(t, targetFile, `export function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

const broken = NonExistentThing;

export function capitalize(str: string): string {
  if (str.length === 0) return str;
  return str.charAt(0).toUpperCase() + str.slice(1);
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
`)

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

func TestTypeScript_Definition(t *testing.T) {
	instance := setupTypeScriptDaemon(t)
	targetFile := filepath.Join(typescriptProjectDir(t), "src", "lib", "utils.ts")

	// "Math.min" on line 2: Math starts at col 10, min starts at col 15.
	// Use col 15 to target the "min" method specifically.
	result := instance.definitionWithRetry(t, targetFile, 2, 15, 30*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for Math.min")
	}

	loc := result.Locations[0]
	// Math.min is defined in a TypeScript lib .d.ts file.
	if !strings.Contains(loc.File, ".d.ts") {
		t.Errorf("expected definition in TypeScript lib .d.ts file, got %q", loc.File)
	}
	if !strings.Contains(loc.Context, "min") {
		t.Errorf("expected context to contain 'min', got %q", loc.Context)
	}
}

func TestTypeScript_FixError(t *testing.T) {
	instance := setupTypeScriptDaemon(t)
	targetFile := filepath.Join(typescriptProjectDir(t), "src", "lib", "utils.ts")

	// Read original content.
	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := `export function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

const broken = NonExistentThing;

export function capitalize(str: string): string {
  if (str.length === 0) return str;
  return str.charAt(0).toUpperCase() + str.slice(1);
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
`
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
