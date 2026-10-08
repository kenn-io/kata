package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"

	"go.kenn.io/kata/internal/httpurl"
)

// TodoistSyncConfig selects daemon-owned credentials. Bindings contain no secrets.
type TodoistSyncConfig struct {
	APIOrigin string `toml:"api_origin"`
	TokenEnv  string `toml:"token_env"`
}

var todoistTokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NormalizeTodoistSyncConfig pins the token to Todoist's public API. Literal
// HTTP loopback origins support isolated local API fixtures.
func NormalizeTodoistSyncConfig(c TodoistSyncConfig) (TodoistSyncConfig, error) {
	c.TokenEnv = strings.TrimSpace(c.TokenEnv)
	if c.TokenEnv == "" {
		c.TokenEnv = "KATA_TODOIST_TOKEN"
	}
	if !todoistTokenEnvPattern.MatchString(c.TokenEnv) {
		return TodoistSyncConfig{}, errors.New("todoist_sync.token_env must be a valid environment variable name")
	}
	if strings.TrimSpace(c.APIOrigin) == "" {
		c.APIOrigin = "https://api.todoist.com"
	}
	origin, err := httpurl.CanonicalHTTPOrigin(c.APIOrigin)
	if err != nil {
		return TodoistSyncConfig{}, errors.New("invalid todoist_sync.api_origin")
	}
	u, _ := url.Parse(origin)
	ip := net.ParseIP(u.Hostname())
	if origin != "https://api.todoist.com" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return TodoistSyncConfig{}, errors.New("todoist_sync.api_origin requires https://api.todoist.com or literal loopback HTTP")
	}
	c.APIOrigin = origin
	return c, nil
}
