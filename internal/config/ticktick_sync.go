package config

import (
	"fmt"
	"regexp"
	"strings"
)

var tickTickTokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TickTickSyncConfig selects a daemon-owned credential; bindings never carry it.
type TickTickSyncConfig struct {
	TokenEnv string `toml:"token_env"`
}

// NormalizeTickTickSyncConfig defaults and validates without resolving secrets.
func NormalizeTickTickSyncConfig(c TickTickSyncConfig) (TickTickSyncConfig, error) {
	c.TokenEnv = strings.TrimSpace(c.TokenEnv)
	if c.TokenEnv == "" {
		c.TokenEnv = "KATA_TICKTICK_TOKEN"
	}
	if !tickTickTokenEnvPattern.MatchString(c.TokenEnv) {
		return TickTickSyncConfig{}, fmt.Errorf("ticktick_sync.token_env must name an environment variable")
	}
	return c, nil
}
