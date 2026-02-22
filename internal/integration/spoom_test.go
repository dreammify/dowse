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

// spoomSetup holds the shared daemon instance for all Spoom tests.
// Sorbet LSP takes a few seconds to start; we share a daemon to avoid
// repeated cold starts.
var (
	spoomInstance *dowseInstance
	spoomHome     string
	spoomOnce     sync.Once
	spoomSetupErr error
)

// teardownSpoom stops the shared Spoom daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownSpoom() {
	if spoomInstance != nil {
		cmd := exec.Command(spoomInstance.binary, "stop")
		cmd.Env = spoomInstance.env
		_ = cmd.Run()
	}
	if spoomHome != "" {
		os.RemoveAll(spoomHome)
	}
	// Clean up .dowse.toml written into the submodule.
	if spoomTomlPath != "" {
		os.Remove(spoomTomlPath)
	}
}

var spoomTomlPath string

func setupSpoomDaemon(t *testing.T) *dowseInstance {
	t.Helper()

	if _, err := exec.LookPath("srb"); err != nil {
		t.Skip("srb not found on PATH, skipping Spoom integration tests")
	}

	spoomOnce.Do(func() {
		binary := buildDowse(t)

		root := projectRoot(t)
		projectDir := filepath.Join(root, "sample-projects", "spoom")

		// Write .dowse.toml into the spoom project root.
		spoomTomlPath = filepath.Join(projectDir, ".dowse.toml")
		tomlContent := "[[lsp]]\nextensions = [\".rb\", \".rbi\"]\ncommand = [\"env\", \"SRB_SKIP_GEM_RBIS=1\", \"srb\", \"tc\", \"--lsp\", \"--disable-watchman\"]\n"
		if err := os.WriteFile(spoomTomlPath, []byte(tomlContent), 0o644); err != nil {
			spoomSetupErr = fmt.Errorf("writing .dowse.toml: %v", err)
			return
		}

		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			spoomSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		spoomHome = dir
		spoomInstance = newDowseInstance(binary, spoomHome)

		// Start the daemon.
		cmd := exec.Command(spoomInstance.binary, "start")
		cmd.Env = spoomInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			spoomSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// Sorbet LSP initialization and indexing.
		warmupFile := filepath.Join(projectDir, "lib", "spoom", "version.rb")
		warmupCmd := exec.Command(spoomInstance.binary, "diagnostics", "--timeout", "180s", warmupFile)
		warmupCmd.Env = spoomInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			spoomSetupErr = fmt.Errorf("spoom warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if spoomSetupErr != nil {
		if strings.Contains(spoomSetupErr.Error(), "srb not found") {
			t.Skipf("skipping: %v", spoomSetupErr)
		}
		t.Fatalf("spoom daemon setup failed: %v", spoomSetupErr)
	}
	return spoomInstance
}

func spoomProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "spoom")
}

func TestSpoom_CleanFile(t *testing.T) {
	instance := setupSpoomDaemon(t)
	cleanFile := filepath.Join(spoomProjectDir(t), "lib", "spoom", "version.rb")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestSpoom_Definition must run before tests that modify source files.
func TestSpoom_Definition(t *testing.T) {
	instance := setupSpoomDaemon(t)
	targetFile := filepath.Join(spoomProjectDir(t), "lib", "spoom", "printer.rb")

	// "Colorize" on line 8, character 13 (1-indexed, inside "include Colorize").
	// Should resolve to lib/spoom/colors.rb where the Colorize module is defined.
	result := instance.definitionWithRetry(t, targetFile, 8, 13, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for Colorize")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "colors.rb") {
		t.Errorf("expected definition in colors.rb, got %q", loc.File)
	}
}

func TestSpoom_TypeMismatch(t *testing.T) {
	instance := setupSpoomDaemon(t)
	targetFile := filepath.Join(spoomProjectDir(t), "lib", "spoom", "version.rb")

	// Inject a type mismatch: use T.let to declare a String, then assign an Integer.
	withModifiedFile(t, targetFile, `# typed: strict
# frozen_string_literal: true

module Spoom
  VERSION = T.let(42, String)
end
`)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "expected") || strings.Contains(lower, "type mismatch") || strings.Contains(lower, "incompatible") || strings.Contains(lower, "7002") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected type mismatch diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestSpoom_UnresolvedReference(t *testing.T) {
	instance := setupSpoomDaemon(t)
	targetFile := filepath.Join(spoomProjectDir(t), "lib", "spoom", "version.rb")

	// Inject an unresolved constant reference.
	withModifiedFile(t, targetFile, `# typed: strict
# frozen_string_literal: true

module Spoom
  VERSION = "1.7.11"
  BrokenConst = NonExistentClass.new
end
`)

	result := instance.diagnosticsWithRetry(t, targetFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "unable to resolve") || strings.Contains(lower, "unresolved") || strings.Contains(lower, "5002") || strings.Contains(lower, "does not exist") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'unable to resolve constant' diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestSpoom_FixError(t *testing.T) {
	instance := setupSpoomDaemon(t)
	targetFile := filepath.Join(spoomProjectDir(t), "lib", "spoom", "version.rb")

	// Read original content.
	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := `# typed: strict
# frozen_string_literal: true

module Spoom
  VERSION = "1.7.11"
  BrokenConst = NonExistentClass.new
end
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
