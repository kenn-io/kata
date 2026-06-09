package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// LocalConfigFilename is the per-developer override file. Gitignored.
const LocalConfigFilename = ".kata.local.toml"

// ErrLocalConfigMissing is returned by ReadLocalConfig when the file
// is absent. Other I/O and parse errors are returned as-is.
var ErrLocalConfigMissing = errors.New(".kata.local.toml not found")

// ReadLocalConfig parses <workspaceRoot>/.kata.local.toml and validates
// version == 1. Unlike ReadProjectConfig, [project] is optional — a
// developer may set only [server]. Empty [server].url is treated as
// the zero value.
func ReadLocalConfig(workspaceRoot string) (*ProjectConfig, error) {
	path := filepath.Join(workspaceRoot, LocalConfigFilename)
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrLocalConfigMissing
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg ProjectConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported .kata.local.toml version %d (expected 1)", cfg.Version)
	}
	cfg.Project.LegacyIdentity = strings.TrimSpace(cfg.Project.LegacyIdentity)
	cfg.Project.Name = strings.TrimSpace(cfg.Project.Name)
	if cfg.Project.Name != "" {
		if err := ValidateProjectName(cfg.Project.Name); err != nil {
			return nil, fmt.Errorf("project.name: %w", err)
		}
	}
	cfg.Server.URL = strings.TrimSpace(cfg.Server.URL)
	cfg.Server.Daemon = strings.TrimSpace(cfg.Server.Daemon)
	return &cfg, nil
}

// MergeLocal overlays non-empty fields from local onto a copy of base.
// Pass nil for "no local file" — base is returned unchanged.
func MergeLocal(base, local *ProjectConfig) *ProjectConfig {
	return MergeLocalWithStderr(base, local, os.Stderr)
}

// MergeLocalWithStderr is MergeLocal with an explicit warning sink kept for
// compatibility with callers/tests. Legacy project.identity values in local
// config are ignored.
func MergeLocalWithStderr(base, local *ProjectConfig, stderr io.Writer) *ProjectConfig {
	_ = stderr
	merged := *base
	if local == nil {
		return &merged
	}
	if local.Project.Name != "" {
		merged.Project.Name = local.Project.Name
	}
	if local.Server.URL != "" {
		merged.Server.URL = local.Server.URL
		merged.Server.Daemon = ""
	}
	if local.Server.Daemon != "" {
		merged.Server.Daemon = local.Server.Daemon
		merged.Server.URL = ""
	}
	return &merged
}
