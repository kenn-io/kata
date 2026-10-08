package config

import (
	"errors"
	"strings"
)

// TwentySyncConfig pins the task API and its daemon-owned credential selector.
// Bindings contain source identity and presentation, never API keys.
type TwentySyncConfig struct {
	APIOrigin string `toml:"api_origin"`
	WebOrigin string `toml:"web_origin"`
	TokenEnv  string `toml:"token_env"`
}

// NormalizeTwentySyncConfig validates configuration without reading credentials.
func NormalizeTwentySyncConfig(c TwentySyncConfig) (TwentySyncConfig, error) {
	c.TokenEnv = strings.TrimSpace(c.TokenEnv)
	if c.TokenEnv == "" {
		c.TokenEnv = "KATA_TWENTY_TOKEN"
	}
	if !planeTokenEnvPattern.MatchString(c.TokenEnv) {
		return TwentySyncConfig{}, errors.New("twenty_sync.token_env must be a valid environment variable name")
	}
	if strings.TrimSpace(c.APIOrigin) == "" {
		c.APIOrigin = "https://api.twenty.com"
	}
	var err error
	c.APIOrigin, err = CanonicalTwentyOrigin(c.APIOrigin)
	if err != nil {
		return TwentySyncConfig{}, errors.New("twenty_sync.api_origin requires a root HTTPS origin or loopback HTTP origin")
	}
	if strings.TrimSpace(c.WebOrigin) == "" {
		c.WebOrigin = c.APIOrigin
		if c.APIOrigin == "https://api.twenty.com" {
			c.WebOrigin = "https://app.twenty.com"
		}
	}
	c.WebOrigin, err = CanonicalTwentyOrigin(c.WebOrigin)
	if err != nil {
		return TwentySyncConfig{}, errors.New("twenty_sync.web_origin requires a root HTTPS origin or loopback HTTP origin")
	}
	return c, nil
}

// CanonicalTwentyOrigin applies the strict root-origin policy shared with Plane.
func CanonicalTwentyOrigin(value string) (string, error) {
	origin, err := CanonicalPlaneOrigin(value)
	if err != nil {
		return "", errors.New("invalid Twenty origin: requires root HTTPS or literal loopback HTTP")
	}
	return origin, nil
}
