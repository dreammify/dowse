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

// okhttpSetup holds the shared daemon instance for all OkHttp tests.
// Kotlin LSP takes 60-120s for cold start, so we share a single daemon.
var (
	okhttpInstance *dowseInstance
	okhttpHome     string
	okhttpOnce     sync.Once
	okhttpSetupErr error
)

// teardownOkHttp stops the shared OkHttp daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownOkHttp() {
	if okhttpInstance != nil {
		cmd := exec.Command(okhttpInstance.binary, "stop")
		cmd.Env = okhttpInstance.env
		_ = cmd.Run()
	}
	if okhttpHome != "" {
		os.RemoveAll(okhttpHome)
	}
	if okhttpTomlPath != "" {
		os.Remove(okhttpTomlPath)
	}
}

var okhttpTomlPath string

func setupOkHttpDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	t.Skip("OkHttp tests disabled: kotlin-lsp does not support semantic analysis for Kotlin Multiplatform projects")

	if _, err := exec.LookPath("kotlin-lsp"); err != nil {
		t.Skip("kotlin-lsp not found on PATH, skipping OkHttp integration tests")
	}

	okhttpOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "okhttp")

		// Write .dowse.toml into the project root.
		okhttpTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".kt\"]\ncommand = [\"kotlin-lsp\", \"--stdio\"]\n"
		if err := os.WriteFile(okhttpTomlPath, []byte(tomlContent), 0o644); err != nil {
			okhttpSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			okhttpSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		okhttpHome = dir
		okhttpInstance = newDowseInstance(binary, okhttpHome)

		// Start the daemon.
		cmd := exec.Command(okhttpInstance.binary, "start")
		cmd.Env = okhttpInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			okhttpSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// Kotlin LSP initialization (JVM + Gradle indexing).
		// OkHttp is a large multiplatform project, so we use a generous timeout.
		warmupFile := filepath.Join(projectDir,
			"okhttp", "src", "jvmMain", "kotlin", "okhttp3", "internal", "platform", "PlatformRegistry.kt")
		warmupCmd := exec.Command(okhttpInstance.binary, "diagnostics", "--timeout", "300s", warmupFile)
		warmupCmd.Env = okhttpInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			okhttpSetupErr = fmt.Errorf("okhttp warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if okhttpSetupErr != nil {
		t.Fatalf("okhttp daemon setup failed: %v", okhttpSetupErr)
	}
	return okhttpInstance
}

func okhttpProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "okhttp")
}

func TestOkHttp_CleanFile(t *testing.T) {
	instance := setupOkHttpDaemon(t)
	cleanFile := filepath.Join(okhttpProjectDir(t),
		"okhttp", "src", "jvmMain", "kotlin", "okhttp3", "internal", "platform", "PlatformRegistry.kt")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestOkHttp_SyntaxError injects a syntax error and verifies it is detected.
// Kotlin LSP reports syntax-level diagnostics for OkHttp's multiplatform project.
func TestOkHttp_SyntaxError(t *testing.T) {
	instance := setupOkHttpDaemon(t)
	targetFile := filepath.Join(okhttpProjectDir(t),
		"okhttp", "src", "jvmMain", "kotlin", "okhttp3", "internal", "platform", "PlatformRegistry.kt")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject a statement between import and object declaration, breaking syntax.
	brokenContent := strings.Replace(string(originalContent),
		"import java.security.Security",
		"val syntaxBreaker = 1\nimport java.security.Security",
		1)
	withModifiedFile(t, targetFile, brokenContent)

	result := instance.diagnosticsWithRetry(t, targetFile, 120*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "imports") || strings.Contains(lower, "syntax") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected syntax diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestOkHttp_FixError(t *testing.T) {
	instance := setupOkHttpDaemon(t)
	targetFile := filepath.Join(okhttpProjectDir(t),
		"okhttp", "src", "jvmMain", "kotlin", "okhttp3", "internal", "platform", "PlatformRegistry.kt")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject a syntax error.
	brokenContent := strings.Replace(string(originalContent),
		"import java.security.Security",
		"val syntaxBreaker = 1\nimport java.security.Security",
		1)
	if err := os.WriteFile(targetFile, []byte(brokenContent), 0o644); err != nil {
		t.Fatalf("writing broken file: %v", err)
	}
	t.Cleanup(func() {
		os.WriteFile(targetFile, originalContent, 0o644)
	})

	// Wait for error to be detected.
	instance.diagnosticsWithRetry(t, targetFile, 120*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	// Fix the error by restoring original content.
	if err := os.WriteFile(targetFile, originalContent, 0o644); err != nil {
		t.Fatalf("restoring file: %v", err)
	}

	// Wait for diagnostics to clear.
	result := instance.diagnosticsWithRetry(t, targetFile, 120*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount == 0 },
		"--timeout", "30s",
	)

	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors after fix, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}
