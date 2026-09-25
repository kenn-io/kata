package client

import (
	"context"
	"errors"
	"fmt"

	internalclient "go.kenn.io/kata/internal/client"
)

// ErrDaemonUnavailable reports that Discover found no configured remote and no
// reachable local daemon.
var ErrDaemonUnavailable = errors.New("no reachable kata daemon")

// DiscoverOptions controls how Discover selects a daemon.
type DiscoverOptions struct {
	// Workspace is the directory where the upward .kata.local.toml lookup
	// starts. Empty uses the process working directory.
	Workspace string
}

// Discover returns a client for the daemon the kata CLI would use: KATA_SERVER,
// then .kata.local.toml [server].url, then active_daemon, then the running
// local daemon. Credentials follow the same source, so a configured remote
// uses its catalog or global token and the local daemon uses KATA_AUTH_TOKEN or
// [auth].token.
//
// Unlike the CLI, Discover never starts, restarts, or version-checks a local
// daemon. When nothing is configured and no local daemon answers, it returns
// an error matching ErrDaemonUnavailable. Configured remotes are not probed;
// the first request establishes reachability.
func Discover(ctx context.Context, discover DiscoverOptions, opts ...Option) (*Client, error) {
	resolved, ok, err := internalclient.DiscoverResolvedInWorkspace(ctx, discover.Workspace)
	if errors.Is(err, internalclient.ErrLocalDaemonUnreachable) {
		return nil, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	if err != nil {
		return nil, fmt.Errorf("discover kata daemon: %w", err)
	}
	if !ok {
		return nil, ErrDaemonUnavailable
	}
	merged := collectOptions(opts...)
	httpClient, err := internalclient.NewHTTPClientForResolved(ctx, resolved, internalOpts(merged.transport))
	if err != nil {
		return nil, err
	}
	merged.httpClient = httpClient
	return newGeneratedClient(resolved.BaseURL, merged)
}
