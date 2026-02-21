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

// sorbetSetup holds the shared daemon instance for all Sorbet tests.
// Sorbet LSP takes a few seconds to start, but we share a daemon for consistency
// with the Kotlin test pattern.
var (
	sorbetInstance *dowseInstance
	sorbetHome     string
	sorbetOnce     sync.Once
	sorbetSetupErr error
)

// teardownSorbet stops the shared Sorbet daemon and removes its home directory.
// Called from TestMain after all tests complete.
func teardownSorbet() {
	if sorbetInstance != nil {
		cmd := exec.Command(sorbetInstance.binary, "stop")
		cmd.Env = sorbetInstance.env
		_ = cmd.Run()
	}
	if sorbetHome != "" {
		os.RemoveAll(sorbetHome)
	}
}

func setupSorbetDaemon(t *testing.T) *dowseInstance {
	t.Helper()
	sorbetOnce.Do(func() {
		if _, err := exec.LookPath("srb"); err != nil {
			sorbetSetupErr = fmt.Errorf("srb not found on PATH: %v", err)
			return
		}

		binary := buildDowse(t)

		// Create DOWSE_HOME manually instead of using setupDowseHome(t),
		// because t.Cleanup would delete the directory when the first test
		// finishes, breaking all subsequent tests that share this daemon.
		// Cleanup is handled by teardownSorbet in TestMain.
		dir, err := os.MkdirTemp("", "dh")
		if err != nil {
			sorbetSetupErr = fmt.Errorf("creating DOWSE_HOME: %v", err)
			return
		}
		sorbetHome = dir
		sorbetInstance = newDowseInstance(binary, sorbetHome)

		// Start the daemon.
		cmd := exec.Command(sorbetInstance.binary, "start")
		cmd.Env = sorbetInstance.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			sorbetSetupErr = fmt.Errorf("dowse start: %v\n%s", err, out)
			return
		}

		// Warm up: query a clean file with a long timeout to wait for
		// Sorbet LSP initialization.
		root := projectRoot(t)
		warmupFile := filepath.Join(root, "sample-projects", "ruby",
			"lib", "core", "string_utils.rb")
		warmupCmd := exec.Command(sorbetInstance.binary, "diagnostics", "--timeout", "120s", warmupFile)
		warmupCmd.Env = sorbetInstance.env
		warmupOut, warmupErr := warmupCmd.CombinedOutput()
		if warmupErr != nil {
			sorbetSetupErr = fmt.Errorf("sorbet warmup: %v\n%s", warmupErr, warmupOut)
		}
	})
	if sorbetSetupErr != nil {
		if strings.Contains(sorbetSetupErr.Error(), "srb not found") {
			t.Skipf("skipping: %v", sorbetSetupErr)
		}
		t.Fatalf("sorbet daemon setup failed: %v", sorbetSetupErr)
	}
	return sorbetInstance
}

func sorbetProjectDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(projectRoot(t), "sample-projects", "ruby")
}

func TestSorbet_CleanFile(t *testing.T) {
	instance := setupSorbetDaemon(t)
	cleanFile := filepath.Join(sorbetProjectDir(t), "lib", "core", "string_utils.rb")

	result := instance.diagnostics(t, cleanFile, "--timeout", "30s")
	if result.ErrorCount != 0 {
		t.Fatalf("expected 0 errors on clean file, got %d: %+v", result.ErrorCount, result.Diagnostics)
	}
}

// TestSorbet_Definition must run before tests that modify source files.
func TestSorbet_Definition(t *testing.T) {
	instance := setupSorbetDaemon(t)
	targetFile := filepath.Join(sorbetProjectDir(t), "lib", "app", "user_service.rb")

	// "capitalize_words" on line 22, character 43 (1-indexed, inside the method name).
	// Should resolve to lib/core/string_utils.rb where the method is defined.
	result := instance.definitionWithRetry(t, targetFile, 22, 43, 60*time.Second,
		func(result DefinitionResult) bool { return len(result.Locations) > 0 },
	)

	if len(result.Locations) == 0 {
		t.Fatal("expected at least one definition location for capitalize_words")
	}

	loc := result.Locations[0]
	if !strings.Contains(loc.File, "string_utils.rb") {
		t.Errorf("expected definition in string_utils.rb, got %q", loc.File)
	}
	if !strings.Contains(loc.Context, "capitalize_words") {
		t.Errorf("expected context to contain 'capitalize_words', got %q", loc.Context)
	}
}

func TestSorbet_TypeMismatch(t *testing.T) {
	instance := setupSorbetDaemon(t)
	userFile := filepath.Join(sorbetProjectDir(t), "lib", "models", "user.rb")

	// Inject a type mismatch: change validate to return an Integer instead of Core::Result.
	withModifiedFile(t, userFile, `# typed: strict
# frozen_string_literal: true

require_relative '../core/entity'
require_relative '../core/identifiable'
require_relative 'permission'

module Models
  class User < Core::Entity
    extend T::Sig
    include Core::Identifiable

    sig { returns(Integer) }
    attr_reader :id

    sig { returns(String) }
    attr_reader :name

    sig { returns(String) }
    attr_reader :email

    sig { returns(Permission) }
    attr_reader :permission

    sig { params(id: Integer, name: String, email: String, permission: Permission).void }
    def initialize(id, name, email, permission)
      super()
      @id = T.let(id, Integer)
      @name = T.let(name, String)
      @email = T.let(email, String)
      @permission = T.let(permission, Permission)
    end

    sig { override.returns(Core::Result) }
    def validate
      42
    end
  end
end
`)

	result := instance.diagnosticsWithRetry(t, userFile, 60*time.Second,
		func(result DiagnosticsResult) bool { return result.ErrorCount > 0 },
		"--timeout", "30s",
	)

	found := false
	for _, diag := range result.Diagnostics {
		lower := strings.ToLower(diag.Message)
		if strings.Contains(lower, "expected") || strings.Contains(lower, "type mismatch") || strings.Contains(lower, "7002") || strings.Contains(lower, "return") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected type mismatch diagnostic, got: %+v", result.Diagnostics)
	}
}

func TestSorbet_UnresolvedReference(t *testing.T) {
	instance := setupSorbetDaemon(t)
	targetFile := filepath.Join(sorbetProjectDir(t), "lib", "core", "string_utils.rb")

	// Prepend an unresolved constant reference.
	withModifiedFile(t, targetFile, `# typed: strict
# frozen_string_literal: true

module Core
  module StringUtils
    extend T::Sig

    BrokenConst = NonExistentClass.new

    sig { params(text: String).returns(String) }
    def self.capitalize_words(text)
      text.split(" ").map(&:capitalize).join(" ")
    end

    sig { params(text: String, max_length: Integer, ellipsis: String).returns(String) }
    def self.truncate(text, max_length, ellipsis = "...")
      if text.length <= max_length
        text
      else
        T.must(text[0, max_length - ellipsis.length]) + ellipsis
      end
    end
  end
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

func TestSorbet_FixError(t *testing.T) {
	instance := setupSorbetDaemon(t)
	targetFile := filepath.Join(sorbetProjectDir(t), "lib", "core", "string_utils.rb")

	// Read original content.
	originalContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading %s: %v", targetFile, err)
	}

	// Inject an error.
	brokenContent := `# typed: strict
# frozen_string_literal: true

module Core
  module StringUtils
    extend T::Sig

    BrokenConst = NonExistentClass.new

    sig { params(text: String).returns(String) }
    def self.capitalize_words(text)
      text.split(" ").map(&:capitalize).join(" ")
    end

    sig { params(text: String, max_length: Integer, ellipsis: String).returns(String) }
    def self.truncate(text, max_length, ellipsis = "...")
      if text.length <= max_length
        text
      else
        T.must(text[0, max_length - ellipsis.length]) + ellipsis
      end
    end
  end
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
