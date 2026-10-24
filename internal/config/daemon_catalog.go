package config

import (
	"fmt"
	"net/url"
	"os"
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

// ClientToken returns the bearer credential a client sends to this entry: its
// token, or the value of the variable token_env names. An unset or empty
// token_env variable returns a *TokenEnvError, so a client never falls back to
// a credential meant for another daemon.
func (d CatalogDaemonConfig) ClientToken() (string, error) {
	if d.TokenEnv == "" {
		return d.Token, nil
	}
	if token := strings.TrimSpace(os.Getenv(d.TokenEnv)); token != "" {
		return token, nil
	}
	return "", &TokenEnvError{Daemon: d.Name, Env: d.TokenEnv}
}

// HasClientToken reports that the entry configures token or token_env. It
// does not read the token_env variable.
func (d CatalogDaemonConfig) HasClientToken() bool {
	return d.Token != "" || d.TokenEnv != ""
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
