package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultConnectorTimeout = 30 * time.Second
	maxConnectorTimeout     = 300
	maxConnectorArgs        = 64
)

var (
	connectorIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ConnectorConfig is one operator-configured connector process.
type ConnectorConfig struct {
	ID             string            `toml:"id"`
	Command        string            `toml:"command"`
	Args           []string          `toml:"args"`
	TimeoutSeconds int               `toml:"timeout_seconds"`
	Env            map[string]string `toml:"env"`
	Settings       map[string]any    `toml:"settings"`
}

// Timeout returns the configured connector-call timeout or its safe default.
func (c ConnectorConfig) Timeout() time.Duration {
	if c.TimeoutSeconds == 0 {
		return defaultConnectorTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// NormalizeConnectorConfigs validates operator-controlled connector process
// configuration and returns canonical instance IDs.
func NormalizeConnectorConfigs(configs []ConnectorConfig) ([]ConnectorConfig, error) {
	normalized := make([]ConnectorConfig, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for i, cfg := range configs {
		cfg.ID = strings.ToLower(strings.TrimSpace(cfg.ID))
		cfg.Command = strings.TrimSpace(cfg.Command)
		if cfg.ID == "" {
			return nil, fmt.Errorf("connector: id is required")
		}
		if !connectorIDPattern.MatchString(cfg.ID) {
			return nil, fmt.Errorf("connector %q: invalid id", cfg.ID)
		}
		if !filepath.IsAbs(cfg.Command) {
			return nil, fmt.Errorf("connector %q: command must be absolute", cfg.ID)
		}
		if len(cfg.Args) > maxConnectorArgs {
			return nil, fmt.Errorf("connector %q: at most %d args are allowed", cfg.ID, maxConnectorArgs)
		}
		if cfg.TimeoutSeconds < 0 || cfg.TimeoutSeconds > maxConnectorTimeout {
			return nil, fmt.Errorf("connector %q: timeout_seconds must be between 0 and %d", cfg.ID, maxConnectorTimeout)
		}
		for target, source := range cfg.Env {
			if !environmentNamePattern.MatchString(target) || !environmentNamePattern.MatchString(source) {
				return nil, fmt.Errorf("connector %q: invalid environment name", cfg.ID)
			}
		}
		if _, err := json.Marshal(cfg.Settings); err != nil {
			return nil, fmt.Errorf("connector %q: settings must be JSON-compatible: %w", cfg.ID, err)
		}
		if _, ok := seen[cfg.ID]; ok {
			return nil, fmt.Errorf("connector %q: duplicate id", cfg.ID)
		}
		seen[cfg.ID] = struct{}{}
		normalized[i] = cfg
	}
	return normalized, nil
}
