package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MCPConfig opts an authoritative server daemon into its /mcp endpoint.
// Empty Projects follows the active catalog; Actor is the static-token fallback.
type MCPConfig struct {
	Enabled  bool     `toml:"enabled"`
	Actor    string   `toml:"actor"`
	Projects []string `toml:"projects"`
}

// Validate is daemon-startup validation, independent of client config readers.
func (c MCPConfig) Validate() error {
	if c.Actor != "" {
		if err := ValidateProjectName(c.Actor); err != nil || !utf8.ValidString(c.Actor) || strings.TrimSpace(c.Actor) != c.Actor {
			return fmt.Errorf("mcp.actor must be printable without surrounding whitespace")
		}
	}
	seen := make(map[string]bool, len(c.Projects))
	for _, name := range c.Projects {
		if err := ValidateProjectName(name); err != nil || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
			return fmt.Errorf("mcp.projects contains an invalid project name")
		}
		if seen[name] {
			return fmt.Errorf("mcp.projects contains duplicate project %q", name)
		}
		seen[name] = true
	}
	return nil
}
