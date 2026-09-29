package config

import (
	"errors"
	"regexp"
	"strings"
)

// NotionSyncConfig holds the daemon-owned credential environment selector.
type NotionSyncConfig struct {
	TokenEnv string `toml:"token_env"`
}

var notionTokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NormalizeNotionSyncConfig defaults and validates without resolving secrets.
func NormalizeNotionSyncConfig(cfg NotionSyncConfig) (NotionSyncConfig, error) {
	cfg.TokenEnv = strings.TrimSpace(cfg.TokenEnv)
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = "KATA_NOTION_TOKEN"
	}
	if !notionTokenEnvPattern.MatchString(cfg.TokenEnv) {
		return NotionSyncConfig{}, errors.New("notion_sync.token_env must be a valid environment variable name")
	}
	return cfg, nil
}
