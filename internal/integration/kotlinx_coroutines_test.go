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

// kotlinxCoroutinesSetup holds the shared daemon instance for all kotlinx.coroutines tests.
// Kotlin LSP takes 60-120s for cold start, so we share a single daemon.
var (
	kotlinxCoroutinesInstance *dowseInstance
	kotlinxCoroutinesHome     string
	kotlinxCoroutinesOnce     sync.Once
	kotlinxCoroutinesSetupErr error
)

// teardownKotlinxCoroutines stops the shared kotlinx.coroutines daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownKotlinxCoroutines() {
	if kotlinxCoroutinesInstance != nil {
		cmd := exec.Command(kotlinxCoroutinesInstance.binary, "stop")
		cmd.Env = kotlinxCoroutinesInstance.env
		_ = cmd.Run()
	}
	if kotlinxCoroutinesHome != "" {
		os.RemoveAll(kotlinxCoroutinesHome)
	}
	if kotlinxCoroutinesTomlPath != "" {
		os.Remove(kotlinxCoroutinesTomlPath)
	}
}

var kotlinxCoroutinesTomlPath string

func setupKotlinxCoroutinesDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("kotlin-lsp"); err != nil {
		t.Skip("kotlin-lsp not found on PATH, skipping kotlinx.coroutines integration tests")
	}

	kotlinxCoroutinesOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "kotlinx.coroutines")

		// Write .dowse.toml into the project root.
		kotlinxCoroutinesTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".kt\"]\ncommand = [\"kotlin-lsp\", \"--stdio\"]\n"
		if err := os.WriteFile(kotlinxCoroutinesTomlPath, []byte(tomlContent), 0o644); err != nil {
			kotlinxCoroutinesSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			kotlinxCoroutinesSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		kotlinxCoroutinesHome = dir
		kotlinxCoroutinesInstance = newDowseInstance(binary, kotlinxCoroutinesHome)

		// Start the daemon.
		cmd := exec.Command(kotlinxCoroutinesInstance.binary, "start")
		cmd.Env = kotlinxCoroutinesInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			kotlinxCoroutinesSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// Kotlin LSP initialization (JVM + Gradle indexing).
		warmupFile := filepath.Join(projectDir,
			"kotlinx-coroutines-core", "common", "src", "CoroutineName.kt")
		warmupCmd := exec.Command(kotlinxCoroutinesInstance.binary, "diagnostics", "--timeout", "180s", warmupFile)
		warmupCmd.Env = kotlinxCoroutinesInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			kotlinxCoroutinesSetupErr = fmt.Errorf("kotlinx.coroutines warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if kotlinxCoroutinesSetupErr != nil {
		t.Fatalf("kotlinx.coroutines daemon setup failed: %v", kotlinxCoroutinesSetupErr)
	}
	return kotlinxCoroutinesInstance
}

func kotlinxCoroutinesProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "kotlinx.coroutines")
}

func TestKotlinxCoroutines_CleanFile(t *testing.T) {
	instance := setupKotlinxCoroutinesDaemon(t)
	cleanFile := filepath.Join(kotlinxCoroutinesProjectDir(t),
		"kotlinx-coroutines-core", "common", "src", "CoroutineName.kt")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestKotlinxCoroutines_Definition must run before tests that modify CoroutineName.kt.
func TestKotlinxCoroutines_Definition(t *testing.T) {
	instance := setupKotlinxCoroutinesDaemon(t)
	targetFile := filepath.Join(kotlinxCoroutinesProjectDir(t),
		"kotlinx-coroutines-core", "common", "src", "Builders.common.kt")

	// "Job" on line 48, character 4 (the return type of the launch function).
	// Should resolve to Job.kt.
	result := instance.definitionWithRetry(t, targetFile, 48, 4, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for Job")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "Job.kt") {
		t.Errorf("expected definition in Job.kt, got %q", loc.File)
	}
}

func TestKotlinxCoroutines_TypeMismatch(t *testing.T) {
	instance := setupKotlinxCoroutinesDaemon(t)
	targetFile := filepath.Join(kotlinxCoroutinesProjectDir(t),
		"kotlinx-coroutines-core", "common", "src", "CoroutineName.kt")

	withModifiedFile(t, targetFile, `package kotlinx.coroutines

import kotlin.coroutines.AbstractCoroutineContextElement
import kotlin.coroutines.CoroutineContext

val name: String = 42

public data class CoroutineName(
    val name: String
) : AbstractCoroutineContextElement(CoroutineName) {
    public companion object Key : CoroutineContext.Key<CoroutineName>
    override fun toString(): String = "CoroutineName($name)"
}
`)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "type mismatch") || strings.Contains(lower, "initializer type mismatch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected type mismatch diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestKotlinxCoroutines_UnresolvedReference(t *testing.T) {
	instance := setupKotlinxCoroutinesDaemon(t)
	targetFile := filepath.Join(kotlinxCoroutinesProjectDir(t),
		"kotlinx-coroutines-core", "common", "src", "CoroutineName.kt")

	withModifiedFile(t, targetFile, `package kotlinx.coroutines

import kotlin.coroutines.AbstractCoroutineContextElement
import kotlin.coroutines.CoroutineContext

val broken: NonExistentType = TODO()

public data class CoroutineName(
    val name: String
) : AbstractCoroutineContextElement(CoroutineName) {
    public companion object Key : CoroutineContext.Key<CoroutineName>
    override fun toString(): String = "CoroutineName($name)"
}
`)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "unresolved reference") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'unresolved reference' diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestKotlinxCoroutines_FixError(t *testing.T) {
	instance := setupKotlinxCoroutinesDaemon(t)
	targetFile := filepath.Join(kotlinxCoroutinesProjectDir(t),
		"kotlinx-coroutines-core", "common", "src", "CoroutineName.kt")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	brokenContent := `package kotlinx.coroutines

import kotlin.coroutines.AbstractCoroutineContextElement
import kotlin.coroutines.CoroutineContext

val broken: NonExistentType = TODO()

public data class CoroutineName(
    val name: String
) : AbstractCoroutineContextElement(CoroutineName) {
    public companion object Key : CoroutineContext.Key<CoroutineName>
    override fun toString(): String = "CoroutineName($name)"
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
