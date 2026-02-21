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

// mockitoSetup holds the shared daemon instance for all Mockito tests.
// jdtls cold start is 1-3 minutes, so we share a single daemon.
var (
	mockitoInstance *dowseInstance
	mockitoHome     string
	mockitoOnce     sync.Once
	mockitoSetupErr error
)

// teardownMockito stops the shared Mockito daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownMockito() {
	if mockitoInstance != nil {
		cmd := exec.Command(mockitoInstance.binary, "stop")
		cmd.Env = mockitoInstance.env
		_ = cmd.Run()
	}
	if mockitoHome != "" {
		os.RemoveAll(mockitoHome)
	}
	if mockitoTomlPath != "" {
		os.Remove(mockitoTomlPath)
	}
}

var mockitoTomlPath string

func setupMockitoDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("jdtls"); err != nil {
		t.Skip("jdtls not found on PATH, skipping Mockito integration tests")
	}

	mockitoOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		// .dowse.toml goes into mockito-core (Gradle subproject with source).
		projectDir := filepath.Join(root, "sample-projects", "mockito", "mockito-core")

		mockitoTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".java\"]\ncommand = [\"jdtls\"]\n"
		if err := os.WriteFile(mockitoTomlPath, []byte(tomlContent), 0o644); err != nil {
			mockitoSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			mockitoSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		mockitoHome = dir
		mockitoInstance = newDowseInstance(binary, mockitoHome)

		// Start the daemon.
		cmd := exec.Command(mockitoInstance.binary, "start")
		cmd.Env = mockitoInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			mockitoSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a very long timeout for
		// jdtls initialization (JVM + Gradle project import + indexing).
		warmupFile := filepath.Join(projectDir,
			"src", "main", "java", "org", "mockito", "ArgumentCaptor.java")
		warmupCmd := exec.Command(mockitoInstance.binary, "diagnostics", "--timeout", "480s", warmupFile)
		warmupCmd.Env = mockitoInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			mockitoSetupErr = fmt.Errorf("mockito warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if mockitoSetupErr != nil {
		t.Fatalf("mockito daemon setup failed: %v", mockitoSetupErr)
	}
	return mockitoInstance
}

func mockitoProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "mockito", "mockito-core")
}

func TestMockito_CleanFile(t *testing.T) {
	instance := setupMockitoDaemon(t)
	cleanFile := filepath.Join(mockitoProjectDir(t),
		"src", "main", "java", "org", "mockito", "ArgumentCaptor.java")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestMockito_Definition must run before tests that modify Mockito.java.
func TestMockito_Definition(t *testing.T) {
	instance := setupMockitoDaemon(t)
	targetFile := filepath.Join(mockitoProjectDir(t),
		"src", "main", "java", "org", "mockito", "Mockito.java")

	// "MockitoCore" on line 9, character 29 (inside "import org.mockito.internal.MockitoCore").
	// Should resolve to internal/MockitoCore.java.
	result := instance.definitionWithRetry(t, targetFile, 9, 29, 120*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for MockitoCore")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "MockitoCore") {
		t.Errorf("expected definition in MockitoCore source, got %q", loc.File)
	}
}

func TestMockito_TypeMismatch(t *testing.T) {
	instance := setupMockitoDaemon(t)
	targetFile := filepath.Join(mockitoProjectDir(t),
		"src", "main", "java", "org", "mockito", "ArgumentCaptor.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject a type mismatch inside the class body.
	brokenContent := strings.Replace(string(originalContent),
		"private final CapturingMatcher<T> capturingMatcher;",
		"private final CapturingMatcher<T> capturingMatcher;\n    String x = 42;",
		1)
	withModifiedFile(t, targetFile, brokenContent)

	result := instance.diagnosticsWithRetry(t, targetFile, 120*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "type mismatch") || strings.Contains(lower, "cannot convert") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected type mismatch diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestMockito_UnresolvedReference(t *testing.T) {
	instance := setupMockitoDaemon(t)
	targetFile := filepath.Join(mockitoProjectDir(t),
		"src", "main", "java", "org", "mockito", "ArgumentCaptor.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an unresolved reference inside the class body.
	brokenContent := strings.Replace(string(originalContent),
		"private final CapturingMatcher<T> capturingMatcher;",
		"private final CapturingMatcher<T> capturingMatcher;\n    NonExistentType x = null;",
		1)
	withModifiedFile(t, targetFile, brokenContent)

	result := instance.diagnosticsWithRetry(t, targetFile, 120*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "cannot be resolved") || strings.Contains(lower, "nonexistenttype") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected unresolved reference diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestMockito_FixError(t *testing.T) {
	instance := setupMockitoDaemon(t)
	targetFile := filepath.Join(mockitoProjectDir(t),
		"src", "main", "java", "org", "mockito", "ArgumentCaptor.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := strings.Replace(string(originalContent),
		"private final CapturingMatcher<T> capturingMatcher;",
		"private final CapturingMatcher<T> capturingMatcher;\n    NonExistentType x = null;",
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
