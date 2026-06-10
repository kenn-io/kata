package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
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
// The target URL is ALWAYS the binding's hub URL (in.hubURL); a catalog entry
// supplies only the admin TOKEN, never the origin, so the admin token cannot be
// redirected to a foreign host. allow_insecure for the hub client likewise
// comes from the binding (in.allowInsecure), not the catalog.
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
	if cat != nil {
		if e := catalogByName(cat, in.hubName); e != nil {
			token, err := selectedCatalogToken(e)
			if err != nil {
				return hubAdminAuth{}, err
			}
			out.token = token
			return out, nil
		}
		if e := catalogByURL(cat, out.url); e != nil {
			token, err := selectedCatalogToken(e)
			if err != nil {
				return hubAdminAuth{}, err
			}
			out.token = token
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

func catalogByURL(cat *config.DaemonConfig, url string) *config.CatalogDaemonConfig {
	if url == "" {
		return nil
	}
	for i := range cat.Daemons {
		if strings.TrimRight(cat.Daemons[i].URL, "/") == url {
			return &cat.Daemons[i]
		}
	}
	return nil
}

// hubAdminClient builds an HTTP client for the resolved hub admin auth.
func hubAdminClient(ctx context.Context, a hubAdminAuth) (*http.Client, error) {
	opts := clientpkg.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout), AllowInsecure: a.allowInsecure}
	// Always NewHTTPClientForTarget: the supplied TargetAuth is the COMPLETE
	// bearer policy for the hub origin. With no resolved hub credential the
	// client is unauthenticated — never fall back to NewHTTPClient, which
	// would attach the local daemon's global KATA_AUTH_TOKEN/[auth].token to
	// the (stored, potentially hostile) hub URL.
	return clientpkg.NewHTTPClientForTarget(ctx, a.url, clientpkg.TargetAuth{Token: strings.TrimSpace(a.token), AllowInsecure: a.allowInsecure}, opts)
}
