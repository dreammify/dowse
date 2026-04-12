package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadWithPaths(t *testing.T) {
	tests := []struct {
		name       string
		global     string // TOML content; empty means no file
		workspace  string // TOML content; empty means no file
		wantErr    bool
		assertFunc func(t *testing.T, cfg *Config)
	}{
		{
			name: "global only",
			global: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			wantErr: false,
			assertFunc: func(t *testing.T, cfg *Config) {
				if len(cfg.LSPs) != 1 {
					t.Fatalf("expected 1 LSP entry, got %d", len(cfg.LSPs))
				}
				assertLSP(t, cfg.LSPs[0], []string{".go"}, []string{"gopls", "serve"})
			},
		},
		{
			name: "workspace only",
			workspace: `
[[lsp]]
extensions = [".kt", ".kts"]
command = ["kotlin-lsp"]
`,
			wantErr: false,
			assertFunc: func(t *testing.T, cfg *Config) {
				if len(cfg.LSPs) != 1 {
					t.Fatalf("expected 1 LSP entry, got %d", len(cfg.LSPs))
				}
				assertLSP(t, cfg.LSPs[0], []string{".kt", ".kts"}, []string{"kotlin-lsp"})
			},
		},
		{
			name: "merge with override",
			global: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]

[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`,
			workspace: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "-remote=auto"]
`,
			wantErr: false,
			assertFunc: func(t *testing.T, cfg *Config) {
				if len(cfg.LSPs) != 2 {
					t.Fatalf("expected 2 LSP entries, got %d", len(cfg.LSPs))
				}
				goLSP, err := cfg.Resolve(".go")
				if err != nil {
					t.Fatal(err)
				}
				assertLSP(t, *goLSP, []string{".go"}, []string{"gopls", "-remote=auto"})

				ktLSP, err := cfg.Resolve(".kt")
				if err != nil {
					t.Fatal(err)
				}
				assertLSP(t, *ktLSP, []string{".kt"}, []string{"kotlin-lsp"})
			},
		},
		{
			name: "merge with addition",
			global: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			workspace: `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`,
			wantErr: false,
			assertFunc: func(t *testing.T, cfg *Config) {
				if len(cfg.LSPs) != 2 {
					t.Fatalf("expected 2 LSP entries, got %d", len(cfg.LSPs))
				}
				goLSP, err := cfg.Resolve(".go")
				if err != nil {
					t.Fatal(err)
				}
				assertLSP(t, *goLSP, []string{".go"}, []string{"gopls", "serve"})

				ktLSP, err := cfg.Resolve(".kt")
				if err != nil {
					t.Fatal(err)
				}
				assertLSP(t, *ktLSP, []string{".kt"}, []string{"kotlin-lsp"})
			},
		},
		{
			name: "merge preserves initialization_options from global",
			global: `
[[lsp]]
extensions = [".java"]
command = ["jdtls"]

[lsp.initialization_options]
bundles = []
extendedClientCapabilities = {classFileContentsSupport = true}
`,
			workspace: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			wantErr: false,
			assertFunc: func(t *testing.T, cfg *Config) {
				javaLSP, err := cfg.Resolve(".java")
				if err != nil {
					t.Fatal(err)
				}
				assertLSP(t, *javaLSP, []string{".java"}, []string{"jdtls"})
				if javaLSP.InitializationOptions == nil {
					t.Fatal("expected non-nil InitializationOptions after merge, got nil")
				}
				if _, ok := javaLSP.InitializationOptions["extendedClientCapabilities"]; !ok {
					t.Fatal("expected extendedClientCapabilities in InitializationOptions after merge")
				}
			},
		},
		{
			name:    "neither exists",
			wantErr: true,
		},
		{
			name:    "malformed global TOML",
			global:  `[[lsp]\nextensions = "not an array"`,
			wantErr: true,
		},
		{
			name:      "malformed workspace TOML",
			workspace: `this is not valid toml {{{`,
			wantErr:   true,
		},
		{
			name: "valid global with malformed workspace",
			global: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			workspace: `this is not valid toml {{{`,
			wantErr:   true,
		},
		{
			name:   "malformed global with valid workspace",
			global: `this is not valid toml {{{`,
			workspace: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			wantErr: true,
		},
		{
			name: "daemon config with session_ttl",
			global: `
[daemon]
session_ttl = "1h"

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			assertFunc: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL() != time.Hour {
					t.Fatalf("expected 1h, got %v", cfg.SessionTTL())
				}
			},
		},
		{
			name: "no daemon section returns default TTL",
			global: `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			assertFunc: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL() != DefaultSessionTTL {
					t.Fatalf("expected %v, got %v", DefaultSessionTTL, cfg.SessionTTL())
				}
			},
		},
		{
			name: "invalid session_ttl returns default TTL",
			global: `
[daemon]
session_ttl = "not-a-duration"

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			assertFunc: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL() != DefaultSessionTTL {
					t.Fatalf("expected %v fallback, got %v", DefaultSessionTTL, cfg.SessionTTL())
				}
			},
		},
		{
			name: "merge workspace daemon overrides global daemon",
			global: `
[daemon]
session_ttl = "1h"

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			workspace: `
[daemon]
session_ttl = "15m"

[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`,
			assertFunc: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL() != 15*time.Minute {
					t.Fatalf("expected 15m (workspace override), got %v", cfg.SessionTTL())
				}
			},
		},
		{
			name: "merge global daemon used when workspace has none",
			global: `
[daemon]
session_ttl = "2h"

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`,
			workspace: `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`,
			assertFunc: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL() != 2*time.Hour {
					t.Fatalf("expected 2h (from global), got %v", cfg.SessionTTL())
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			var globalPath, workspacePath string
			if tt.global != "" {
				globalPath = writeFile(t, dir, "global.toml", tt.global)
			}
			if tt.workspace != "" {
				workspacePath = writeFile(t, dir, "workspace.toml", tt.workspace)
			}

			cfg, err := LoadWithPaths(globalPath, workspacePath)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.assertFunc != nil {
				tt.assertFunc(t, cfg)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	cfg := &Config{
		LSPs: []LSPConfig{
			{Extensions: []string{".go"}, Command: []string{"gopls", "serve"}},
			{Extensions: []string{".kt", ".kts"}, Command: []string{"kotlin-lsp"}},
		},
	}

	tests := []struct {
		name    string
		ext     string
		wantCmd []string
		wantErr bool
	}{
		{
			name:    "resolve hit",
			ext:     ".go",
			wantCmd: []string{"gopls", "serve"},
		},
		{
			name:    "resolve hit multi-extension",
			ext:     ".kts",
			wantCmd: []string{"kotlin-lsp"},
		},
		{
			name:    "resolve miss",
			ext:     ".rs",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lsp, err := cfg.Resolve(tt.ext)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertStringSlice(t, "command", lsp.Command, tt.wantCmd)
		})
	}
}

func TestFindNearest(t *testing.T) {
	validTOML := `[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`
	globalTOML := `[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`

	tests := []struct {
		name       string
		setup      func(t *testing.T, root string, globalPath string) (absPath string)
		wantRoot   string // relative to root; empty means root itself
		wantErr    bool
		globalTOML string // content for global config; empty means no global
	}{
		{
			name:       "config in file parent dir",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				sub := filepath.Join(root, "sub")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sub, ".dowse.toml"), []byte(validTOML), 0o644); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(sub, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantRoot: "sub",
		},
		{
			name:       "config at gitRoot",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				if err := os.WriteFile(filepath.Join(root, ".dowse.toml"), []byte(validTOML), 0o644); err != nil {
					t.Fatal(err)
				}
				sub := filepath.Join(root, "sub")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(sub, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantRoot: "",
		},
		{
			name:       "config at intermediate dir",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				intermediate := filepath.Join(root, "a")
				if err := os.MkdirAll(filepath.Join(intermediate, "b", "c"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(intermediate, ".dowse.toml"), []byte(validTOML), 0o644); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(intermediate, "b", "c", "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantRoot: "a",
		},
		{
			name:       "nearest wins over gitRoot",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				if err := os.WriteFile(filepath.Join(root, ".dowse.toml"), []byte(validTOML), 0o644); err != nil {
					t.Fatal(err)
				}
				sub := filepath.Join(root, "sub")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sub, ".dowse.toml"), []byte(validTOML), 0o644); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(sub, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantRoot: "sub",
		},
		{
			name:       "no config anywhere uses global only",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				sub := filepath.Join(root, "sub")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(sub, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantRoot: "",
		},
		{
			name: "no config at all",
			setup: func(t *testing.T, root, _ string) string {
				filePath := filepath.Join(root, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantErr: true,
		},
		{
			name:       "malformed config",
			globalTOML: globalTOML,
			setup: func(t *testing.T, root, _ string) string {
				if err := os.WriteFile(filepath.Join(root, ".dowse.toml"), []byte("this is not valid toml {{{"), 0o644); err != nil {
					t.Fatal(err)
				}
				filePath := filepath.Join(root, "main.kt")
				if err := os.WriteFile(filePath, []byte("fun main() {}"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			var globalPath string
			if tt.globalTOML != "" {
				globalPath = writeFile(t, root, "global-config.toml", tt.globalTOML)
			}

			absPath := tt.setup(t, root, globalPath)

			result, err := FindNearest(absPath, root, globalPath)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			expectedRoot := root
			if tt.wantRoot != "" {
				expectedRoot = filepath.Join(root, tt.wantRoot)
			}
			if result.ProjectRoot != expectedRoot {
				t.Fatalf("expected project root %q, got %q", expectedRoot, result.ProjectRoot)
			}
			if result.Config == nil {
				t.Fatal("expected non-nil config")
			}
		})
	}
}

func TestInitializationOptions(t *testing.T) {
	dir := t.TempDir()
	workspacePath := writeFile(t, dir, "workspace.toml", `
[[lsp]]
extensions = [".java"]
command = ["jdtls"]

[lsp.initialization_options]
bundles = []
extendedClientCapabilities = {classFileContentsSupport = true}
`)
	cfg, err := LoadWithPaths("", workspacePath)
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}
	lsp, err := cfg.Resolve(".java")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lsp.InitializationOptions == nil {
		t.Fatal("expected non-nil InitializationOptions")
	}
	if _, ok := lsp.InitializationOptions["extendedClientCapabilities"]; !ok {
		t.Fatal("expected extendedClientCapabilities in InitializationOptions")
	}
}

func TestMaxSeverity(t *testing.T) {
	tests := []struct {
		level    string
		expected int
	}{
		{"error", 1},
		{"warning", 2},
		{"information", 3},
		{"hint", 4},
		{"", 1},
		{"invalid", 1},
		{"Error", 1}, // case-sensitive: unknown → default
	}

	for _, testCase := range tests {
		t.Run(testCase.level, func(t *testing.T) {
			cfg := &LSPConfig{Level: testCase.level}
			if got := cfg.MaxSeverity(); got != testCase.expected {
				t.Errorf("MaxSeverity() for level %q: got %d, want %d", testCase.level, got, testCase.expected)
			}
		})
	}
}

func TestMergePreservesLevel(t *testing.T) {
	dir := t.TempDir()
	globalPath := writeFile(t, dir, "global.toml", `
[[lsp]]
extensions = [".go"]
command = ["gopls"]
level = "warning"
`)
	workspacePath := writeFile(t, dir, "workspace.toml", `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
level = "hint"
`)

	cfg, err := LoadWithPaths(globalPath, workspacePath)
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}

	goLSP, err := cfg.Resolve(".go")
	if err != nil {
		t.Fatalf("Resolve .go: %v", err)
	}
	if goLSP.Level != "warning" {
		t.Errorf("expected .go level 'warning', got %q", goLSP.Level)
	}

	ktLSP, err := cfg.Resolve(".kt")
	if err != nil {
		t.Fatalf("Resolve .kt: %v", err)
	}
	if ktLSP.Level != "hint" {
		t.Errorf("expected .kt level 'hint', got %q", ktLSP.Level)
	}
}

func TestMCPConfigParsing(t *testing.T) {
	dir := t.TempDir()
	globalPath := writeFile(t, dir, "global.toml", `
[mcp]
tools = ["get_diagnostics", "get_definition"]

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`)

	cfg, err := LoadWithPaths(globalPath, "")
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}

	tools := cfg.ExposedTools()
	if len(tools) != 2 {
		t.Fatalf("ExposedTools() count = %d, want 2", len(tools))
	}
	if tools[0] != "get_diagnostics" || tools[1] != "get_definition" {
		t.Errorf("ExposedTools() = %v, want [get_diagnostics get_definition]", tools)
	}
}

func TestMCPConfigAbsent(t *testing.T) {
	dir := t.TempDir()
	globalPath := writeFile(t, dir, "global.toml", `
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`)

	cfg, err := LoadWithPaths(globalPath, "")
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}

	if cfg.MCP != nil {
		t.Errorf("expected nil MCP config, got %+v", cfg.MCP)
	}
	if tools := cfg.ExposedTools(); tools != nil {
		t.Errorf("ExposedTools() = %v, want nil", tools)
	}
}

func TestMCPConfigMerge(t *testing.T) {
	dir := t.TempDir()
	globalPath := writeFile(t, dir, "global.toml", `
[mcp]
tools = ["get_diagnostics"]

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`)
	workspacePath := writeFile(t, dir, "workspace.toml", `
[mcp]
tools = ["get_definition", "get_diagnostics_batch"]

[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`)

	cfg, err := LoadWithPaths(globalPath, workspacePath)
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}

	tools := cfg.ExposedTools()
	if len(tools) != 2 {
		t.Fatalf("ExposedTools() count = %d, want 2 (workspace override)", len(tools))
	}
	if tools[0] != "get_definition" || tools[1] != "get_diagnostics_batch" {
		t.Errorf("ExposedTools() = %v, want [get_definition get_diagnostics_batch]", tools)
	}
}

func TestMCPConfigMergeGlobalUsedWhenWorkspaceAbsent(t *testing.T) {
	dir := t.TempDir()
	globalPath := writeFile(t, dir, "global.toml", `
[mcp]
tools = ["get_diagnostics"]

[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]
`)
	workspacePath := writeFile(t, dir, "workspace.toml", `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
`)

	cfg, err := LoadWithPaths(globalPath, workspacePath)
	if err != nil {
		t.Fatalf("LoadWithPaths: %v", err)
	}

	tools := cfg.ExposedTools()
	if len(tools) != 1 || tools[0] != "get_diagnostics" {
		t.Errorf("ExposedTools() = %v, want [get_diagnostics] (from global)", tools)
	}
}

func assertLSP(t *testing.T, got LSPConfig, wantExts, wantCmd []string) {
	t.Helper()
	assertStringSlice(t, "extensions", got.Extensions, wantExts)
	assertStringSlice(t, "command", got.Command, wantCmd)
}

func assertStringSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d]: got %q, want %q", label, i, got[i], want[i])
		}
	}
}

func TestRestartOnConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("parses restart_on from workspace config", func(t *testing.T) {
		wsPath := writeFile(t, dir, "restart_on.toml", `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp", "--stdio"]
restart_on = ["**/*.gradle.kts", "**/build.gradle"]
`)
		cfg, err := LoadWithPaths("", wsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.LSPs) != 1 {
			t.Fatalf("expected 1 LSP entry, got %d", len(cfg.LSPs))
		}
		assertStringSlice(t, "restart_on", cfg.LSPs[0].RestartOn, []string{"**/*.gradle.kts", "**/build.gradle"})
	})

	t.Run("empty restart_on is valid", func(t *testing.T) {
		wsPath := writeFile(t, dir, "no_restart.toml", `
[[lsp]]
extensions = [".go"]
command = ["gopls"]
`)
		cfg, err := LoadWithPaths("", wsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.LSPs[0].RestartOn) != 0 {
			t.Fatalf("expected empty restart_on, got %v", cfg.LSPs[0].RestartOn)
		}
	})

	t.Run("merge preserves restart_on from global config", func(t *testing.T) {
		globalPath := writeFile(t, dir, "global_restart.toml", `
[[lsp]]
extensions = [".go"]
command = ["gopls"]
restart_on = ["**/go.mod", "**/go.sum"]
`)
		wsPath := writeFile(t, dir, "ws_restart.toml", `
[[lsp]]
extensions = [".kt"]
command = ["kotlin-lsp"]
restart_on = ["**/*.gradle.kts"]
`)
		cfg, err := LoadWithPaths(globalPath, wsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.LSPs) != 2 {
			t.Fatalf("expected 2 LSP entries, got %d", len(cfg.LSPs))
		}
		// Workspace entry first, then global.
		assertStringSlice(t, "ws restart_on", cfg.LSPs[0].RestartOn, []string{"**/*.gradle.kts"})
		assertStringSlice(t, "global restart_on", cfg.LSPs[1].RestartOn, []string{"**/go.mod", "**/go.sum"})
	})
}
