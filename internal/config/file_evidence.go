package config

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

var fileEvidenceAliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidFileEvidenceSourceAlias reports whether a source has a portable alias.
func ValidFileEvidenceSourceAlias(alias string) bool {
	return fileEvidenceAliasPattern.MatchString(alias)
}

// FileEvidenceSource configures a local root or trusted read-only adapter.
// It contains only settings; reading and process execution belong to the daemon.
type FileEvidenceSource struct {
	Root    string   `toml:"root"`
	Command []string `toml:"command"`
	SHA256  bool     `toml:"sha256"`
	// Env explicitly grants daemon environment variable names to the adapter.
	Env []string `toml:"env"`
}

// FileEvidenceConfig grants source publication authority to the selected daemon.
type FileEvidenceConfig struct {
	Sources map[string]FileEvidenceSource `toml:"sources"`
}

// Validate checks settings without contacting sources or loading a resolver.
func (c FileEvidenceConfig) Validate() error {
	for alias, s := range c.Sources {
		if !ValidFileEvidenceSourceAlias(alias) || (s.Root == "") == (len(s.Command) == 0) || (s.Root != "" && !filepath.IsAbs(s.Root)) {
			return errors.New("file_evidence sources require valid aliases and exactly one absolute root or command argv")
		}
		if len(s.Command) > 0 && strings.TrimSpace(s.Command[0]) == "" {
			return errors.New("file_evidence command must not be empty")
		}
		for _, name := range s.Env {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return errors.New("file_evidence env must contain variable names")
			}
		}
	}
	return nil
}
