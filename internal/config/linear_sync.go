package config

import (
	"fmt"
	"regexp"
	"strings"
)

// LinearSyncConfig selects daemon-owned credentials for the fixed Linear API.
type LinearSyncConfig struct {
	TokenEnv string `toml:"token_env"`
	AuthType string `toml:"auth_type"`
}

// NormalizeLinearSyncConfig validates selectors without resolving credentials.
func NormalizeLinearSyncConfig(c LinearSyncConfig) (LinearSyncConfig, error) {
	c.TokenEnv = strings.TrimSpace(c.TokenEnv)
	if c.TokenEnv == "" {
		c.TokenEnv = "KATA_LINEAR_TOKEN"
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(c.TokenEnv) {
		return LinearSyncConfig{}, fmt.Errorf("linear_sync.token_env must be a valid environment variable name")
	}
	if c.AuthType == "" {
		c.AuthType = "api-key"
	}
	if c.AuthType != "api-key" && c.AuthType != "oauth" {
		return LinearSyncConfig{}, fmt.Errorf("linear_sync.auth_type must be api-key or oauth")
	}
	return c, nil
}
