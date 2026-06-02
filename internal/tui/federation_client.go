package tui

import (
	"context"

	clientpkg "go.kenn.io/kata/internal/client"
	hubfederation "go.kenn.io/kata/internal/federation"
)

func newHubAdminClient(ctx context.Context, target daemonTarget) (*Client, daemonTarget, error) {
	resolved, err := resolveDaemonTargetToken(target)
	if err != nil {
		return nil, daemonTarget{}, err
	}
	var endpoint string
	if resolved.Local {
		endpoint, err = ensureLocalRunningForTUI(ctx)
	} else {
		endpoint, err = normalizeRemoteURLForTUI(resolved.URL, resolved.AllowInsecure)
	}
	if err != nil {
		return nil, daemonTarget{}, err
	}
	hc, err := newHTTPClientForTUI(ctx, endpoint, resolved, clientOptsNormal)
	if err != nil {
		return nil, daemonTarget{}, err
	}
	return NewClient(endpoint, hc), resolvedDaemonTarget(resolved, endpoint), nil
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
