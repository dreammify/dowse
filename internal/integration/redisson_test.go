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

// redissonSetup holds the shared daemon instance for all Redisson tests.
// jdtls cold start is 1-3 minutes, so we share a single daemon.
var (
	redissonInstance *dowseInstance
	redissonHome     string
	redissonOnce     sync.Once
	redissonSetupErr error
)

// teardownRedisson stops the shared Redisson daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownRedisson() {
	if redissonInstance != nil {
		cmd := exec.Command(redissonInstance.binary, "stop")
		cmd.Env = redissonInstance.env
		_ = cmd.Run()
	}
	if redissonHome != "" {
		os.RemoveAll(redissonHome)
	}
	if redissonTomlPath != "" {
		os.Remove(redissonTomlPath)
	}
}

var redissonTomlPath string

func setupRedissonDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("jdtls"); err != nil {
		t.Skip("jdtls not found on PATH, skipping Redisson integration tests")
	}

	redissonOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		// .dowse.toml goes into the redisson core module (where pom.xml lives).
		projectDir := filepath.Join(root, "sample-projects", "redisson", "redisson")

		redissonTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".java\"]\ncommand = [\"jdtls\"]\n"
		if err := os.WriteFile(redissonTomlPath, []byte(tomlContent), 0o644); err != nil {
			redissonSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			redissonSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		redissonHome = dir
		redissonInstance = newDowseInstance(binary, redissonHome)

		// Start the daemon.
		cmd := exec.Command(redissonInstance.binary, "start")
		cmd.Env = redissonInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			redissonSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a very long timeout for
		// jdtls initialization (JVM + Maven project import + indexing).
		warmupFile := filepath.Join(projectDir,
			"src", "main", "java", "org", "redisson", "api", "RLock.java")
		warmupCmd := exec.Command(redissonInstance.binary, "diagnostics", "--timeout", "180s", warmupFile)
		warmupCmd.Env = redissonInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			redissonSetupErr = fmt.Errorf("redisson warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if redissonSetupErr != nil {
		t.Fatalf("redisson daemon setup failed: %v", redissonSetupErr)
	}
	return redissonInstance
}

func redissonProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "redisson", "redisson")
}

func TestRedisson_CleanFile(t *testing.T) {
	instance := setupRedissonDaemon(t)
	cleanFile := filepath.Join(redissonProjectDir(t),
		"src", "main", "java", "org", "redisson", "api", "RLock.java")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestRedisson_Definition must run before tests that modify Redisson.java.
func TestRedisson_Definition(t *testing.T) {
	instance := setupRedissonDaemon(t)
	targetFile := filepath.Join(redissonProjectDir(t),
		"src", "main", "java", "org", "redisson", "Redisson.java")

	// "RedissonClient" on line 57, character 43 (inside "implements RedissonClient").
	// Should resolve to api/RedissonClient.java.
	result := instance.definitionWithRetry(t, targetFile, 57, 43, 120*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for RedissonClient")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "RedissonClient") {
		t.Errorf("expected definition in RedissonClient source, got %q", loc.File)
	}
}

func TestRedisson_TypeMismatch(t *testing.T) {
	instance := setupRedissonDaemon(t)
	targetFile := filepath.Join(redissonProjectDir(t),
		"src", "main", "java", "org", "redisson", "api", "RLock.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject a type mismatch inside the interface body.
	brokenContent := strings.Replace(string(originalContent),
		"String getName();",
		"String getName();\n    String x = 42;",
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

func TestRedisson_UnresolvedReference(t *testing.T) {
	instance := setupRedissonDaemon(t)
	targetFile := filepath.Join(redissonProjectDir(t),
		"src", "main", "java", "org", "redisson", "api", "RLock.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an unresolved reference inside the interface body.
	brokenContent := strings.Replace(string(originalContent),
		"String getName();",
		"String getName();\n    NonExistentType x = null;",
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

func TestRedisson_FixError(t *testing.T) {
	instance := setupRedissonDaemon(t)
	targetFile := filepath.Join(redissonProjectDir(t),
		"src", "main", "java", "org", "redisson", "api", "RLock.java")

	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := strings.Replace(string(originalContent),
		"String getName();",
		"String getName();\n    NonExistentType x = null;",
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
