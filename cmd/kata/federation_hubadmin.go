package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
)

type hubAuthInputs struct {
	hubURL        string
	hubName       string
	hubToken      string
	allowInsecure bool
}

type hubAdminAuth struct {
	url           string
	token         string
	allowInsecure bool
}

// resolveHubAdminAuth applies precedence: --hub-token > named catalog entry
// token/token_env > URL-matched catalog entry token/token_env > no credential
// (empty token here; the caller builds an UNAUTHENTICATED hub client — the
// local daemon's global KATA_AUTH_TOKEN/[auth].token is never sent to the hub
// origin implicitly; a token-protected hub requires --hub-token or a catalog
// entry).
//
// The target URL is ALWAYS the binding's hub URL (in.hubURL), and a catalog
// token is only attached when its entry's URL matches that target: token and
// origin travel together, so neither can redirect the other to a foreign host.
// A --hub <name> entry that is missing or whose URL differs from the binding
// errors out — sending a named entry's admin token to a different origin is
// the cross-origin leak this guards against; --hub-token is the only
// deliberate cross-origin path.
//
// allow_insecure for the hub client is the UNION of the caller's opt-in
// (binding/status or --allow-insecure) and the matched same-origin catalog
// entry's: every source is the operator's own local record for this exact
// origin, so the catalog can restore an opt-in lost with the credential but
// can never remove the binding's.
//
// When a catalog entry IS selected (by --hub <name> or by URL match) but its
// token_env is set and empty, resolution returns an error instead of an empty
// token: a selected-but-unresolvable entry must not silently fall through to
// the global daemon token, which would send the wrong credential to the hub.
func resolveHubAdminAuth(cat *config.DaemonConfig, in hubAuthInputs) (hubAdminAuth, error) {
	out := hubAdminAuth{url: strings.TrimRight(in.hubURL, "/"), allowInsecure: in.allowInsecure}
	if strings.TrimSpace(in.hubToken) != "" {
		out.token = in.hubToken
		return out, nil
	}
	hubOrigin, err := config.CanonicalHTTPOrigin(out.url)
	if err != nil {
		return hubAdminAuth{}, fmt.Errorf("canonicalize spoke hub origin: %w", err)
	}
	if name := strings.TrimSpace(in.hubName); name != "" {
		var e *config.CatalogDaemonConfig
		if cat != nil {
			e = catalogByName(cat, name)
		}
		if e == nil {
			return hubAdminAuth{}, &cliError{
				Message:  fmt.Sprintf("--hub %q does not match any daemon catalog entry", name),
				Code:     "hub_catalog_entry_not_found",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
		entryOrigin, err := config.CanonicalHTTPOrigin(e.URL)
		if err != nil {
			return hubAdminAuth{}, fmt.Errorf("canonicalize --hub %q catalog origin: %w", name, err)
		}
		if entryOrigin != hubOrigin {
			return hubAdminAuth{}, &cliError{
				Message: fmt.Sprintf(
					"--hub %q resolves to %s, not this spoke's hub %s; refusing to send its admin token to a different origin (pass --hub-token to use an explicit token with this hub)",
					name, textsafe.Line(e.URL), textsafe.Line(out.url)),
				Code:     "hub_catalog_url_mismatch",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
		token, err := selectedCatalogToken(e)
		if err != nil {
			return hubAdminAuth{}, err
		}
		out.token = token
		out.allowInsecure = out.allowInsecure || e.AllowInsecure
		return out, nil
	}
	if cat != nil {
		e, err := catalogByOrigin(cat, hubOrigin, out.url)
		if err != nil {
			return hubAdminAuth{}, err
		}
		if e != nil {
			token, err := selectedCatalogToken(e)
			if err != nil {
				return hubAdminAuth{}, err
			}
			out.token = token
			out.allowInsecure = out.allowInsecure || e.AllowInsecure
			return out, nil
		}
	}
	return out, nil
}

// selectedCatalogToken returns the admin token for a catalog entry that was
// actively selected. A literal token wins; otherwise a non-empty token_env is
// read. token_env set but empty is an error so the wrong daemon token is not
// sent — only an entry with NO token configured at all yields the empty-token
// global fallback.
func selectedCatalogToken(e *config.CatalogDaemonConfig) (string, error) {
	if strings.TrimSpace(e.Token) != "" {
		return e.Token, nil
	}
	if env := strings.TrimSpace(e.TokenEnv); env != "" {
		token := strings.TrimSpace(os.Getenv(env))
		if token == "" {
			return "", &cliError{
				Message:  fmt.Sprintf("hub admin token_env %q is set but empty; export it or pass --hub-token", env),
				Code:     "hub_token_env_empty",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
		return token, nil
	}
	return "", nil
}

func catalogByName(cat *config.DaemonConfig, name string) *config.CatalogDaemonConfig {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	for i := range cat.Daemons {
		if cat.Daemons[i].Name == name {
			return &cat.Daemons[i]
		}
	}
	return nil
}

func catalogByOrigin(
	cat *config.DaemonConfig,
	origin string,
	targetURL string,
) (*config.CatalogDaemonConfig, error) {
	if origin == "" {
		return nil, nil
	}
	targetBaseURL, err := canonicalHubBaseURL(targetURL)
	if err != nil {
		return nil, fmt.Errorf("canonicalize spoke hub URL: %w", err)
	}
	var originMatches []*config.CatalogDaemonConfig
	var exactMatches []*config.CatalogDaemonConfig
	for i := range cat.Daemons {
		if strings.TrimSpace(cat.Daemons[i].URL) == "" {
			continue
		}
		entryOrigin, err := config.CanonicalHTTPOrigin(cat.Daemons[i].URL)
		if err != nil {
			return nil, fmt.Errorf(
				"canonicalize daemon catalog entry %q origin: %w",
				cat.Daemons[i].Name, err,
			)
		}
		if entryOrigin != origin {
			continue
		}
		entry := &cat.Daemons[i]
		originMatches = append(originMatches, entry)
		entryBaseURL, err := canonicalHubBaseURL(entry.URL)
		if err != nil {
			return nil, fmt.Errorf(
				"canonicalize daemon catalog entry %q URL: %w",
				entry.Name, err,
			)
		}
		if entryBaseURL == targetBaseURL {
			exactMatches = append(exactMatches, entry)
		}
	}
	switch {
	case len(exactMatches) == 1:
		return exactMatches[0], nil
	case len(exactMatches) > 1:
		return nil, ambiguousHubCatalogOriginError()
	case len(originMatches) == 1:
		return originMatches[0], nil
	case len(originMatches) > 1:
		return nil, ambiguousHubCatalogOriginError()
	default:
		return nil, nil
	}
}

func canonicalHubBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	origin, err := config.CanonicalHTTPOrigin(raw)
	if err != nil {
		return "", err
	}
	baseURL := origin + strings.TrimRight(parsed.EscapedPath(), "/")
	if parsed.ForceQuery || parsed.RawQuery != "" {
		baseURL += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		baseURL += "#" + parsed.EscapedFragment()
	}
	return baseURL, nil
}

func ambiguousHubCatalogOriginError() error {
	return &cliError{
		Message:  "multiple daemon catalog entries match this hub origin; pass --hub to select one",
		Code:     "hub_catalog_origin_ambiguous",
		Kind:     kindValidation,
		ExitCode: ExitValidation,
	}
}

// hubAdminClient builds an HTTP client for the resolved hub admin auth.
func hubAdminClient(ctx context.Context, a hubAdminAuth) (*http.Client, error) {
	opts := clientpkg.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout), AllowInsecure: a.allowInsecure}
	auth, err := config.ReadAuthConfig()
	if err != nil {
		auth.TrustPrivateNetwork = config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	}
	// Always NewHTTPClientForTarget: the supplied TargetAuth is the COMPLETE
	// bearer policy for the hub origin. With no resolved hub credential the
	// client is unauthenticated — never fall back to NewHTTPClient, which
	// would attach the local daemon's global KATA_AUTH_TOKEN/[auth].token to
	// the (stored, potentially hostile) hub URL.
	return clientpkg.NewHTTPClientForTarget(ctx, a.url,
		clientpkg.TargetAuth{
			Token:               strings.TrimSpace(a.token),
			AllowInsecure:       a.allowInsecure,
			TrustPrivateNetwork: auth.TrustPrivateNetwork,
		}, opts)
}
