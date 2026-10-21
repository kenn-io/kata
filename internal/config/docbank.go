package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"unicode"

	"go.kenn.io/kata/internal/httpurl"
)

// DocbankConfig selects a vault shared by every issue reader and metadata writer
// served by this daemon. Folder placement is organization, not authorization.
type DocbankConfig struct {
	URL        string `toml:"url"`
	APIKey     string `toml:"api_key"`
	APIKeyFile string `toml:"api_key_file"`
	APIKeyEnv  string `toml:"api_key_env"`
	Folder     string `toml:"folder"`
}

// Enabled reports whether an operator configured a Docbank endpoint.
func (c DocbankConfig) Enabled() bool { return c.URL != "" }

// NormalizeDocbankConfig applies daemon defaults and validates without reading secrets.
func NormalizeDocbankConfig(c DocbankConfig) (DocbankConfig, error) {
	err := normalizeDocbank(&c)
	return c, err
}

// ResolveKey reads the selected secret on every request, permitting key rotation.
func (c DocbankConfig) ResolveKey() (string, error) {
	key, _, err := ResolveSecret(c.APIKey, c.APIKeyFile, c.APIKeyEnv)
	if err != nil {
		return "", fmt.Errorf("docbank: %w", err)
	}
	if key == "" {
		return "", fmt.Errorf("docbank: API key is required")
	}
	return key, nil
}
func normalizeDocbank(c *DocbankConfig) error {
	c.URL = strings.TrimSpace(c.URL)
	c.Folder = strings.TrimSpace(c.Folder)
	if c.Folder == "" {
		c.Folder = "/kata"
	}
	if !strings.HasPrefix(c.Folder, "/") || path.Clean(c.Folder) != c.Folder || strings.ContainsAny(c.Folder, `\`) || strings.ContainsFunc(c.Folder, unicode.IsControl) {
		return fmt.Errorf("docbank.folder must be a clean absolute path")
	}
	if !c.Enabled() {
		return nil
	}
	base, err := httpurl.CanonicalHTTPBaseURL(c.URL)
	if err != nil {
		return fmt.Errorf("docbank.url: %w", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("docbank.url is invalid")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("docbank.url requires HTTPS except on loopback")
		}
	}
	c.URL = base
	if strings.TrimSpace(c.APIKey) == "" && strings.TrimSpace(c.APIKeyFile) == "" && strings.TrimSpace(c.APIKeyEnv) == "" {
		return fmt.Errorf("docbank: API key is required")
	}
	return nil
}
