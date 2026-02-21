// Package config handles parsing and merging of global (~/.dowse/config.toml)
// and workspace (.dowse.toml) configuration files.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// DaemonConfig holds daemon-level settings.
type DaemonConfig struct {
	SessionTTL string `toml:"session_ttl"` // Go duration string, e.g. "30m", "2h"
}

const DefaultSessionTTL = 30 * time.Minute

// MCPConfig holds MCP server settings.
type MCPConfig struct {
	Tools []string `toml:"tools"` // tool names to expose; nil/absent = all
}

// Config holds the merged LSP configuration.
type Config struct {
	Daemon *DaemonConfig `toml:"daemon"`
	MCP    *MCPConfig    `toml:"mcp"`
	LSPs   []LSPConfig   `toml:"lsp"`
}

// SessionTTL returns the configured session idle TTL, defaulting to 30 minutes.
func (c *Config) SessionTTL() time.Duration {
	if c.Daemon == nil || c.Daemon.SessionTTL == "" {
		return DefaultSessionTTL
	}
	dur, err := time.ParseDuration(c.Daemon.SessionTTL)
	if err != nil {
		return DefaultSessionTTL
	}
	return dur
}

// ExposedTools returns the list of MCP tool names to expose.
// Returns nil if no [mcp] section is configured or tools list is empty,
// meaning all tools should be exposed.
func (c *Config) ExposedTools() []string {
	if c.MCP == nil || len(c.MCP.Tools) == 0 {
		return nil
	}
	return c.MCP.Tools
}

// LSPConfig maps file extensions to an LSP server command.
type LSPConfig struct {
	Extensions            []string       `toml:"extensions"`
	Command               []string       `toml:"command"`
	Level                 string         `toml:"level"`
	InitializationOptions map[string]any `toml:"initialization_options"`
}

// MaxSeverity returns the LSP severity threshold for this config's level.
// LSP severities: 1=error, 2=warning, 3=information, 4=hint.
// Returns 1 (error only) when Level is empty or invalid.
func (l *LSPConfig) MaxSeverity() int {
	switch l.Level {
	case "hint":
		return 4
	case "information":
		return 3
	case "warning":
		return 2
	case "error":
		return 1
	default:
		return 1
	}
}

// LoadWithPaths loads config from specific file paths for testability.
// globalPath and workspacePath can be empty strings if the file doesn't exist.
func LoadWithPaths(globalPath, workspacePath string) (*Config, error) {
	global, globalErr := loadFile(globalPath)
	if globalErr != nil {
		return nil, globalErr
	}
	workspace, workspaceErr := loadFile(workspacePath)
	if workspaceErr != nil {
		return nil, workspaceErr
	}

	if global == nil && workspace == nil {
		return nil, fmt.Errorf("no config found (checked %s and %s)", globalPath, workspacePath)
	}
	if global == nil {
		return workspace, nil
	}
	if workspace == nil {
		return global, nil
	}
	return merge(global, workspace), nil
}

// loadFile parses a single TOML config file. Returns (nil, nil) if the path
// is empty or the file does not exist. Returns a non-nil error only for
// actual failures (e.g. malformed TOML).
func loadFile(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// merge combines global and workspace configs. Workspace entries override
// global entries on a per-extension basis.
func merge(global, workspace *Config) *Config {
	// Build a set of extensions claimed by the workspace config.
	wsExts := make(map[string]bool)
	for _, lsp := range workspace.LSPs {
		for _, ext := range lsp.Extensions {
			wsExts[ext] = true
		}
	}

	// Start with workspace entries, then add global entries for extensions
	// not overridden by the workspace.
	merged := &Config{
		LSPs: make([]LSPConfig, len(workspace.LSPs)),
	}
	copy(merged.LSPs, workspace.LSPs)

	// Workspace daemon config takes precedence over global.
	if workspace.Daemon != nil {
		merged.Daemon = workspace.Daemon
	} else {
		merged.Daemon = global.Daemon
	}

	// Workspace MCP config takes precedence over global.
	if workspace.MCP != nil {
		merged.MCP = workspace.MCP
	} else {
		merged.MCP = global.MCP
	}

	for _, lsp := range global.LSPs {
		var kept []string
		for _, ext := range lsp.Extensions {
			if !wsExts[ext] {
				kept = append(kept, ext)
			}
		}
		if len(kept) > 0 {
			merged.LSPs = append(merged.LSPs, LSPConfig{
				Extensions:            kept,
				Command:               lsp.Command,
				Level:                 lsp.Level,
				InitializationOptions: lsp.InitializationOptions,
			})
		}
	}

	return merged
}

// FindNearestResult holds the result of walking up from a file path to find
// the nearest .dowse.toml config.
type FindNearestResult struct {
	ProjectRoot string
	Config      *Config
}

// FindNearest walks up from absPath to gitRootDir looking for the nearest
// .dowse.toml. The directory containing it becomes the project root. If none
// is found, gitRootDir is used as the fallback project root.
func FindNearest(absPath, gitRootDir, globalConfigPath string) (*FindNearestResult, error) {
	dir := filepath.Dir(absPath)
	for {
		candidate := filepath.Join(dir, ".dowse.toml")
		if fileExists(candidate) {
			cfg, err := LoadWithPaths(globalConfigPath, candidate)
			if err != nil {
				return nil, err
			}
			return &FindNearestResult{ProjectRoot: dir, Config: cfg}, nil
		}
		if dir == gitRootDir {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without hitting gitRootDir.
			break
		}
		dir = parent
	}
	// No .dowse.toml found; fall back to git root with global-only config.
	cfg, err := LoadWithPaths(globalConfigPath, "")
	if err != nil {
		return nil, err
	}
	return &FindNearestResult{ProjectRoot: gitRootDir, Config: cfg}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Resolve returns the LSPConfig for a given file extension, or an error if
// none is configured.
func (c *Config) Resolve(ext string) (*LSPConfig, error) {
	for i := range c.LSPs {
		for _, extension := range c.LSPs[i].Extensions {
			if extension == ext {
				return &c.LSPs[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no LSP configured for extension %q", ext)
}
