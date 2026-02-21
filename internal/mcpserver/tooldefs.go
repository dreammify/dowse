package mcpserver

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed tools/*.toml
var toolsFS embed.FS

// Intermediate TOML parse types.
type toolFile struct {
	Name        string                  `toml:"name"`
	Description string                  `toml:"description"`
	Parameters  map[string]parameterDef `toml:"parameters"`
}

type parameterDef struct {
	Type        string    `toml:"type"`
	Description string    `toml:"description"`
	Required    bool      `toml:"required"`
	Items       *itemsDef `toml:"items"`
}

type itemsDef struct {
	Type string `toml:"type"`
}

// allToolDefs is populated at init from embedded TOML files.
var allToolDefs []ToolDef

func init() {
	defs, err := loadToolDefs()
	if err != nil {
		panic(fmt.Sprintf("loading embedded tool definitions: %v", err))
	}
	allToolDefs = defs
}

func loadToolDefs() ([]ToolDef, error) {
	entries, err := toolsFS.ReadDir("tools")
	if err != nil {
		return nil, err
	}
	var defs []ToolDef
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		data, err := toolsFS.ReadFile("tools/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		var parsed toolFile
		if err := toml.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", entry.Name(), err)
		}
		defs = append(defs, toToolDef(parsed))
	}
	return defs, nil
}

func toToolDef(file toolFile) ToolDef {
	properties := make(map[string]PropertySchema, len(file.Parameters))
	var required []string
	for name, param := range file.Parameters {
		prop := PropertySchema{Type: param.Type, Description: param.Description}
		if param.Items != nil {
			prop.Items = &PropertySchema{Type: param.Items.Type}
		}
		properties[name] = prop
		if param.Required {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return ToolDef{
		Name:        file.Name,
		Description: strings.TrimSpace(file.Description),
		InputSchema: InputSchema{Type: "object", Properties: properties, Required: required},
	}
}
