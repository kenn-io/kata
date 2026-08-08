package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// ErrProjectConfigMissing is returned by ReadProjectConfig when the workspace
// has no .kata.toml at the given path.
var ErrProjectConfigMissing = errors.New(".kata.toml not found")

// ProjectConfigFilename is the canonical filename committed at workspace roots.
const ProjectConfigFilename = ".kata.toml"

// ProjectConfig is the parsed contents of a workspace .kata.toml or
// .kata.local.toml. The same struct serves both files; readers differ
// only in which validations they enforce.
type ProjectConfig struct {
	Version int             `toml:"version"`
	Project ProjectBindings `toml:"project"`
	Server  ServerConfig    `toml:"server,omitempty"`
}

// ProjectBindings carries the [project] block.
type ProjectBindings struct {
	LegacyIdentity string `toml:"identity,omitempty"`
	Name           string `toml:"name"`
}

// ServerConfig carries the [server] block. Optional in both committed
// and local config files. URL is the daemon base URL (e.g.
// http://100.64.0.5:7777). When set on .kata.local.toml it directs
// the client to a remote daemon; ignored if it appears in committed
// .kata.toml in v1, but parsed without error.
//
// AllowInsecure opts out of the client-side scheme guard that rejects
// plain http to a non-private host. Required when URL uses a hostname
// (the client cannot resolve it without DNS access) or a public IP
// over plain http. Has no effect on https URLs.
type ServerConfig struct {
	URL           string `toml:"url,omitempty"`
	AllowInsecure bool   `toml:"allow_insecure,omitempty"`
}

// ReadProjectConfig parses <workspaceRoot>/.kata.toml and validates v1 fields.
// Returns (nil, ErrProjectConfigMissing) when the file does not exist; other
// I/O or validation errors are returned as-is.
func ReadProjectConfig(workspaceRoot string) (*ProjectConfig, error) {
	path := filepath.Join(workspaceRoot, ProjectConfigFilename)
	data, err := os.ReadFile(path) //nolint:gosec // workspace-supplied path is the documented input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProjectConfigMissing
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg ProjectConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported .kata.toml version %d (expected 1)", cfg.Version)
	}
	cfg.Project.LegacyIdentity = strings.TrimSpace(cfg.Project.LegacyIdentity)
	cfg.Project.Name = strings.TrimSpace(cfg.Project.Name)
	if cfg.Project.Name == "" && cfg.Project.LegacyIdentity != "" {
		cfg.Project.Name = lastSegment(cfg.Project.LegacyIdentity)
	}
	if err := ValidateProjectName(cfg.Project.Name); err != nil {
		return nil, fmt.Errorf("project.name: %w", err)
	}
	return &cfg, nil
}

// FindProjectConfig walks upward from startPath looking for a readable
// .kata.toml. Returns the parsed config and the directory it was found
// in. Returns ErrProjectConfigMissing if no .kata.toml is found. In a git
// workspace, discovery stops at the git root; otherwise it stops at the
// filesystem root.
//
// This is the discovery counterpart to ReadProjectConfig. Callers that
// know the exact workspace root should use ReadProjectConfig directly;
// callers running from arbitrary cwds inside a workspace (CLI, TUI)
// use this to find the binding without forcing the user to `cd` first.
func FindProjectConfig(startPath string) (*ProjectConfig, string, error) {
	disc, err := DiscoverPaths(startPath)
	if err != nil {
		return nil, "", err
	}
	if disc.WorkspaceRoot == "" {
		return nil, "", ErrProjectConfigMissing
	}
	cfg, err := ReadProjectConfig(disc.WorkspaceRoot)
	if err != nil {
		return nil, "", err
	}
	return cfg, disc.WorkspaceRoot, nil
}

// WriteProjectConfig writes a v1 .kata.toml at <workspaceRoot>/.kata.toml.
func WriteProjectConfig(workspaceRoot, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidateProjectName(name); err != nil {
		return err
	}
	body := fmt.Sprintf("version = 1\n\n[project]\nname = %q\n", name)
	path := filepath.Join(workspaceRoot, ProjectConfigFilename)
	return os.WriteFile(path, []byte(body), 0o644) //nolint:gosec // committed project file, world-readable by design
}

// ValidateProjectName rejects names that can spoof terminal output when echoed
// by CLI/TUI surfaces. Names may contain printable Unicode and spaces, but not
// control characters, ESC, newlines, or other non-printable runes.
func ValidateProjectName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name must be non-empty")
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("name contains non-printable character %U", r)
		}
	}
	return nil
}

func lastSegment(identity string) string {
	for i := len(identity) - 1; i >= 0; i-- {
		if identity[i] == '/' || identity[i] == ':' {
			return identity[i+1:]
		}
	}
	return identity
}
