package config

import (
	"fmt"
	"net/url"
	"strings"
)

// TokenEnvError reports a catalog entry whose token_env variable is unset or
// empty.
type TokenEnvError struct {
	Daemon string
	Env    string
}

func (e *TokenEnvError) Error() string {
	return fmt.Sprintf("daemon %q: token_env %q is unset or empty", e.Daemon, e.Env)
}

// ClientToken resolves token > token_file > token_env. A failed selected
// source never falls back to another credential.
// An unset or empty selected token_env retains the *TokenEnvError contract.
func (d CatalogDaemonConfig) ClientToken() (string, error) {
	token, source, err := ResolveSecret(d.Token, d.TokenFile, d.TokenEnv)
	if err != nil {
		if strings.HasPrefix(source, "env:") {
			return "", &TokenEnvError{Daemon: d.Name, Env: strings.TrimSpace(d.TokenEnv)}
		}
		return "", fmt.Errorf("daemon %q: %w", d.Name, err)
	}
	return token, nil
}

// HasClientToken reports a configured token selector without reading any
// credential file or environment variable.
func (d CatalogDaemonConfig) HasClientToken() bool {
	return d.Token != "" || d.TokenFile != "" || d.TokenEnv != ""
}

// DisplayEndpoint names where the entry points, for display: "local" for a
// local entry, otherwise the URL's host and port. It never includes URL
// userinfo; a URL without a host gives "".
func (d CatalogDaemonConfig) DisplayEndpoint() string {
	if d.Local {
		return "local"
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// CatalogDaemon returns a copy of the named daemon catalog entry.
func (c *DaemonConfig) CatalogDaemon(name string) (CatalogDaemonConfig, bool) {
	for _, daemon := range c.Daemons {
		if daemon.Name == name {
			return daemon, true
		}
	}
	return CatalogDaemonConfig{}, false
}

// ActiveCatalogDaemon returns the entry active_daemon names.
func (c *DaemonConfig) ActiveCatalogDaemon() (CatalogDaemonConfig, bool) {
	if c.ActiveDaemon == "" {
		return CatalogDaemonConfig{}, false
	}
	return c.CatalogDaemon(c.ActiveDaemon)
}
