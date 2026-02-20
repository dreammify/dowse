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

// kotlinSetup holds the shared daemon instance for all Kotlin tests.
// The Kotlin LSP takes 60-90s for cold start, so we share a single daemon.
var (
	kotlinInstance *dowseInstance
	kotlinHome     string
	kotlinOnce     sync.Once
	kotlinSetupErr error
)

// teardownKotlin stops the shared Kotlin daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownKotlin() {
	if kotlinInstance != nil {
		cmd := exec.Command(kotlinInstance.binary, "stop")
		cmd.Env = kotlinInstance.env
		_ = cmd.Run()
	}
	if kotlinHome != "" {
		os.RemoveAll(kotlinHome)
	}
}

func setupKotlinDaemon(t *testing.T) *dowseInstance {
	t.Helper()
	kotlinOnce.Do(func() {
		binary := buildDowse(t)

		// Create DOWSE_HOME manually instead of using setupDowseHome(t),
		// because t.Cleanup would delete the directory when the first test
		// finishes, breaking all subsequent tests that share this daemon.
		// Cleanup is handled by teardownKotlin in TestMain.
		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			kotlinSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		kotlinHome = dir
		kotlinInstance = newDowseInstance(binary, kotlinHome)

		// Start the daemon.
		cmd := exec.Command(kotlinInstance.binary, "start")
		cmd.Env = kotlinInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			kotlinSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// Kotlin LSP initialization (JVM + Gradle indexing).
		root := projectRoot(t)
		warmupFile := filepath.Join(root, "sample-projects", "kotlin",
			"core", "src", "main", "kotlin", "com", "example", "core", "StringUtils.kt")
		warmupCmd := exec.Command(kotlinInstance.binary, "diagnostics", "--timeout", "120s", warmupFile)
		warmupCmd.Env = kotlinInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			kotlinSetupErr = fmt.Errorf("kotlin warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if kotlinSetupErr != nil {
		t.Fatalf("kotlin daemon setup failed: %v", kotlinSetupErr)
	}
	return kotlinInstance
}

func kotlinProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "kotlin")
}

func TestKotlin_CleanFile(t *testing.T) {
	instance := setupKotlinDaemon(t)
	cleanFile := filepath.Join(kotlinProjectDir(t),
		"core", "src", "main", "kotlin", "com", "example", "core", "StringUtils.kt")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestKotlin_Definition must run before tests that modify StringUtils.kt.
// Even with the file watcher integrated, the Kotlin LSP's cross-file index
// lags behind rapid modify/restore cycles. Tests that mutate StringUtils.kt
// shift line offsets, and the LSP may not fully re-index after restoration
// before the definition query runs.
func TestKotlin_Definition(t *testing.T) {
	instance := setupKotlinDaemon(t)
	targetFile := filepath.Join(kotlinProjectDir(t),
		"app", "src", "main", "kotlin", "com", "example", "app", "Application.kt")

	// "capitalizeWords()" on line 31, character 35 (inside the method name).
	// Should resolve to StringUtils.kt where the extension function is defined.
	result := instance.definitionWithRetry(t, targetFile, 31, 35, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for capitalizeWords")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "StringUtils.kt") {
		t.Errorf("expected definition in StringUtils.kt, got %q", loc.File)
	}
	if loc.Line != 3 {
		t.Errorf("expected definition on line 3, got %d", loc.Line)
	}
	if !strings.Contains(loc.Context, "capitalizeWords") {
		t.Errorf("expected context to contain 'capitalizeWords', got %q", loc.Context)
	}
}

func TestKotlin_TypeMismatch(t *testing.T) {
	instance := setupKotlinDaemon(t)
	userFile := filepath.Join(kotlinProjectDir(t),
		"models", "src", "main", "kotlin", "com", "example", "models", "User.kt")

	// Inject a type mismatch: change the name field type default.
	withModifiedFile(t, userFile, `package com.example.models

import com.example.core.Entity
import com.example.core.Identifiable
import com.example.core.Result

data class User(
    override val id: Long,
    val name: String = 42,
    val email: String,
    val permission: Permission
) : Entity(), Identifiable<Long> {

    override fun validate(): Result<Unit> = when {
        name.isBlank() -> Result.Failure("Name must not be blank")
        !email.contains("@") -> Result.Failure("Invalid email format")
        else -> Result.Success(Unit)
    }
}
`)

	result := instance.diagnosticsWithRetry(t, userFile, 60*time.Second,
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

func TestKotlin_UnresolvedReference(t *testing.T) {
	instance := setupKotlinDaemon(t)
	targetFile := filepath.Join(kotlinProjectDir(t),
		"core", "src", "main", "kotlin", "com", "example", "core", "StringUtils.kt")

	withModifiedFile(t, targetFile, `package com.example.core

val broken: NonExistentType = TODO()

fun String.capitalizeWords(): String =
    split(" ").joinToString(" ") { word ->
        word.replaceFirstChar { it.uppercaseChar() }
    }

fun String.truncate(maxLength: Int, ellipsis: String = "..."): String =
    if (length <= maxLength) this
    else take(maxLength - ellipsis.length) + ellipsis
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

func TestKotlin_FixError(t *testing.T) {
	instance := setupKotlinDaemon(t)
	targetFile := filepath.Join(kotlinProjectDir(t),
		"core", "src", "main", "kotlin", "com", "example", "core", "StringUtils.kt")

	// Inject an error.
	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	brokenContent := `package com.example.core

val broken: NonExistentType = TODO()

fun String.capitalizeWords(): String =
    split(" ").joinToString(" ") { word ->
        word.replaceFirstChar { it.uppercaseChar() }
    }

fun String.truncate(maxLength: Int, ellipsis: String = "..."): String =
    if (length <= maxLength) this
    else take(maxLength - ellipsis.length) + ellipsis
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
