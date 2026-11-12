package config

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/transcript"
)

// AgentsViewConfig selects the serving daemon's optional usage source.
type AgentsViewConfig struct {
	URL        string `toml:"url"`
	BrowserURL string `toml:"browser_url"`
	TokenFile  string `toml:"token_file"`
	TokenEnv   string `toml:"token_env"`
}

// ResolvedAgentsView contains validated targets and a startup-resolved secret.
type ResolvedAgentsView struct{ URL, BrowserURL, Token string }

func (c ResolvedAgentsView) Enabled() bool { return c.URL != "" }

// Resolve leaves a partially configured, disabled source untouched, including
// its secret. Errors intentionally exclude input values and credential paths.
func (c AgentsViewConfig) Resolve(ctx context.Context, trustPrivate bool) (ResolvedAgentsView, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedAgentsView{}, err
	}
	if strings.TrimSpace(c.URL) == "" {
		return ResolvedAgentsView{}, nil
	}
	base, err := httpurl.CanonicalHTTPBaseURL(c.URL)
	if err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] invalid URL")
	}
	if _, err = (configURLValidator{}).validate(base); err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] invalid URL")
	}
	if _, err = (BearerPolicy{TrustPrivateNetwork: trustPrivate}).OriginForBaseURL(base); err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] URL is not a permitted bearer target")
	}
	browser := strings.TrimSpace(c.BrowserURL)
	if browser == "" {
		browser = base
	}
	browser, err = httpurl.CanonicalHTTPBaseURL(browser)
	if err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] invalid browser URL")
	}
	if _, err = (configURLValidator{}).validate(browser); err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] invalid browser URL")
	}
	token, _, err := ResolveSecret("", c.TokenFile, c.TokenEnv)
	if err != nil {
		return ResolvedAgentsView{}, errors.New("[agentsview] selected credential is unavailable")
	}
	return ResolvedAgentsView{URL: base, BrowserURL: browser, Token: token}, nil
}

type configURLValidator struct{}

func (configURLValidator) validate(base string) (string, error) {
	return (transcript.Transcript{Agent: "example", SessionID: "example"}).Link(base)
}
