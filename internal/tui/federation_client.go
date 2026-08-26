package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	hubfederation "go.kenn.io/kata/internal/federation"
)

func newHubAdminClient(ctx context.Context, target daemonTarget) (*Client, daemonTarget, error) {
	hubToken, err := federationHubTargetToken(target)
	if err != nil {
		return nil, daemonTarget{}, err
	}
	resolved := target.resolved
	if resolved.Source == clientpkg.DaemonSourceUnknown && target.Name != "" {
		resolved, err = ensureResolvedNamedForTUI(ctx, target.Name)
		if err == nil && !target.Local {
			var selectedBaseURL string
			selectedBaseURL, err = normalizeRemoteURLForTUI(
				target.URL, target.resolved.AllowInsecure,
			)
			if err == nil && selectedBaseURL != resolved.BaseURL {
				err = fmt.Errorf("daemon %q url changed from %q to %q",
					target.Name, selectedBaseURL, resolved.BaseURL)
			}
			resolved.AllowInsecure = resolved.AllowInsecure || target.resolved.AllowInsecure
		}
	} else if resolved.BaseURL == "" {
		resolved.BaseURL, err = normalizeRemoteURLForTUI(target.URL, resolved.AllowInsecure)
		resolved.Source = clientpkg.DaemonSourceInjected
	}
	if err != nil {
		return nil, daemonTarget{}, err
	}
	if !target.Local {
		// Federation hub credentials are an origin-specific trust boundary.
		// Named resolution supplies exact endpoint metadata, but its ordinary
		// global auth override belongs to daemon switching and must never replace
		// the catalog credential sent to a hub. Injected credential-free targets
		// retain only the global transport trust decision, never its token.
		resolved.Token = hubToken
		if resolved.Source == clientpkg.DaemonSourceInjected {
			resolved.TrustPrivateNetwork = resolved.TrustPrivateNetwork ||
				config.ResolvedBearerTrustPrivateNetwork()
		}
		resolved.BaseURL, err = normalizeRemoteURLForTUI(resolved.BaseURL, resolved.AllowInsecure)
		if err != nil {
			return nil, daemonTarget{}, err
		}
	}
	target.resolved = resolved
	endpoint := resolved.BaseURL
	if target.Local {
		target.URL = ""
	} else {
		target.URL = endpoint
	}
	hc, err := newHTTPClientForTUI(ctx, endpoint, target, clientOptsNormal)
	if err != nil {
		return nil, daemonTarget{}, err
	}
	return NewClient(endpoint, hc), target, nil
}

// federationHubTargetToken resolves only the credential attached to the
// selected hub target. In particular, KATA_AUTH_TOKEN authenticates the local
// daemon and is never a fallback or override for a catalog hub.
func federationHubTargetToken(target daemonTarget) (string, error) {
	if target.TokenEnv == "" {
		return target.resolved.Token, nil
	}
	token := strings.TrimSpace(os.Getenv(target.TokenEnv))
	if token == "" {
		return "", fmt.Errorf("daemon %q: token_env %q is unset or empty",
			daemonTargetDisplay(target), target.TokenEnv)
	}
	return token, nil
}

func validateFederationHubTargetCredential(target daemonTarget) error {
	_, err := federationHubTargetToken(target)
	return err
}

func newHubEnrollmentClient(
	ctx context.Context,
	hubURL string,
	token string,
	allowInsecure bool,
) (federationEnrollmentAPI, error) {
	endpoint, err := normalizeRemoteURLForTUI(hubURL, allowInsecure)
	if err != nil {
		return nil, err
	}
	return hubfederation.NewClient(ctx, endpoint, token,
		clientpkg.Opts{Timeout: defaultHTTPTimeout, AllowInsecure: allowInsecure})
}
