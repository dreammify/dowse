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

// jdtlsSetup holds the shared daemon instance for all jdtls tests.
// jdtls cold start is 1-3 minutes, so we share a single daemon.
var (
	jdtlsInstance *dowseInstance
	jdtlsHome     string
	jdtlsOnce     sync.Once
	jdtlsSetupErr error
)

// teardownJdtls stops the shared jdtls daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownJdtls() {
	if jdtlsInstance != nil {
		cmd := exec.Command(jdtlsInstance.binary, "stop")
		cmd.Env = jdtlsInstance.env
		_ = cmd.Run()
	}
	if jdtlsHome != "" {
		os.RemoveAll(jdtlsHome)
	}
}

func setupJdtlsDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	// Skip if jdtls is not on PATH.
	if _, err := exec.LookPath("jdtls"); err != nil {
		t.Skip("jdtls not found on PATH, skipping jdtls integration tests")
	}

	jdtlsOnce.Do(func() {
		binary := buildDowse(t)

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			jdtlsSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		jdtlsHome = dir
		jdtlsInstance = newDowseInstance(binary, jdtlsHome)

		// Start the daemon.
		cmd := exec.Command(jdtlsInstance.binary, "start")
		cmd.Env = jdtlsInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			jdtlsSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// jdtls initialization (JVM + Gradle project import + indexing).
		root := projectRoot(t)
		warmupFile := filepath.Join(root, "sample-projects", "java",
			"src", "main", "java", "com", "example", "App.java")
		warmupCmd := exec.Command(jdtlsInstance.binary, "diagnostics", "--timeout", "180s", warmupFile)
		warmupCmd.Env = jdtlsInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			jdtlsSetupErr = fmt.Errorf("jdtls warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if jdtlsSetupErr != nil {
		t.Fatalf("jdtls daemon setup failed: %v", jdtlsSetupErr)
	}
	return jdtlsInstance
}

func javaProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "java")
}

func TestJdtls_CleanFile(t *testing.T) {
	instance := setupJdtlsDaemon(t)
	cleanFile := filepath.Join(javaProjectDir(t),
		"src", "main", "java", "com", "example", "App.java")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestJdtls_Definition must run before tests that modify App.java.
// See TestKotlin_Definition comment for the full explanation.
func TestJdtls_Definition(t *testing.T) {
	instance := setupJdtlsDaemon(t)
	targetFile := filepath.Join(javaProjectDir(t),
		"src", "main", "java", "com", "example", "App.java")

	// "greet" on line 5, character 36 (the 'g' in 'Greeter.greet').
	// Should resolve to Greeter.java where the static method is defined.
	result := instance.definitionWithRetry(t, targetFile, 5, 36, 120*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for greet")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "Greeter") {
		t.Errorf("expected definition in Greeter source, got %q", loc.File)
	}
	if !strings.Contains(loc.Context, "greet") {
		t.Errorf("expected context to contain 'greet', got %q", loc.Context)
	}
}

func TestJdtls_TypeMismatch(t *testing.T) {
	instance := setupJdtlsDaemon(t)
	targetFile := filepath.Join(javaProjectDir(t),
		"src", "main", "java", "com", "example", "App.java")

	withModifiedFile(t, targetFile, `package com.example;

public class App {
    public static void main(String[] args) {
        String x = 42;
        System.out.println(x);
    }
}
`)

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

func TestJdtls_UnresolvedReference(t *testing.T) {
	instance := setupJdtlsDaemon(t)
	targetFile := filepath.Join(javaProjectDir(t),
		"src", "main", "java", "com", "example", "App.java")

	withModifiedFile(t, targetFile, `package com.example;

public class App {
    public static void main(String[] args) {
        NonExistentType x = null;
        System.out.println(x);
    }
}
`)

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

func TestJdtls_FixError(t *testing.T) {
	instance := setupJdtlsDaemon(t)
	targetFile := filepath.Join(javaProjectDir(t),
		"src", "main", "java", "com", "example", "App.java")

	// Read original content.
	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := `package com.example;

public class App {
    public static void main(String[] args) {
        NonExistentType x = null;
        System.out.println(x);
    }
}
`
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
