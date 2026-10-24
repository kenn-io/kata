package client

import (
	"context"
	"errors"
	"fmt"

	internalclient "go.kenn.io/kata/internal/client"
)

// ErrDaemonUnavailable reports that Discover found no configured remote and no
// reachable local daemon, or could not start a stopped one with StartWith.
var ErrDaemonUnavailable = errors.New("no reachable kata daemon")

// ErrDaemonNotInCatalog reports that DiscoverOptions.Daemon names no
// [[daemon]] entry in <KATA_HOME>/config.toml.
var ErrDaemonNotInCatalog = internalclient.ErrNamedDaemonNotFound

// DiscoverOptions controls how Discover selects a daemon.
type DiscoverOptions struct {
	// Workspace is the directory where the upward .kata.local.toml lookup
	// starts. Empty uses the process working directory.
	Workspace string
	// Daemon names a daemon catalog entry, as the CLI's global --daemon flag
	// does. It replaces every other selection source, so Workspace is ignored.
	// DaemonCatalog lists the names.
	Daemon string
	// StartWith is the path of a kata executable. When it is set and the
	// selected local daemon has no runtime, Discover runs `kata daemon start`,
	// or `kata daemon recover` for a pinned local profile, then connects. Empty
	// never starts a daemon, and a remote is never started.
	StartWith string
}

// DiscoveredDaemon describes the daemon Discover selected. It carries no
// credentials or endpoint details.
type DiscoveredDaemon struct {
	// Remote reports a configured remote daemon, matching `kata daemon
	// locate`'s kind=remote. Kata treats such a daemon as unable to read the
	// caller's files, even at a loopback address. A pinned local profile is
	// not remote.
	Remote bool
}

// Discover returns a client for the daemon the kata CLI would use: KATA_SERVER,
// then .kata.local.toml [server].url or [server].daemon, then active_daemon,
// then the running default local daemon. The workspace [server].daemon source
// selects a pinned local profile. Credentials follow the same source: a
// configured remote uses its catalog or global token, the default local daemon
// uses KATA_AUTH_TOKEN or the current home's [auth].token, and a local profile
// uses its own auth configuration and any catalog client credential.
//
// DiscoverOptions.Daemon selects one catalog entry instead, as the CLI's
// global --daemon flag does. The entry's token or token_env is the
// credential, except that KATA_AUTH_TOKEN overrides it for a remote entry.
// Otherwise an unset or empty token_env variable is an error. An unknown name
// returns an error matching ErrDaemonNotInCatalog.
//
// Like `kata daemon locate`, Discover never starts the selected daemon unless
// DiscoverOptions.StartWith is set. Use `kata daemon diagnose` and `kata
// daemon recover` to inspect and recover an eligible stopped local profile
// yourself. A selected local daemon without a reachable runtime returns an
// error matching ErrDaemonUnavailable; profile storage and identity validation
// failures keep their profile-specific errors. Configured remotes are not
// probed; the first request establishes reachability. Use Client.Discovered to
// check whether the selected daemon is remote.
func Discover(ctx context.Context, discover DiscoverOptions, opts ...Option) (*Client, error) {
	resolved, ok, err := internalclient.DiscoverSelected(ctx, internalclient.DiscoverRequest{
		WorkspaceStart: discover.Workspace,
		Name:           discover.Daemon,
		StartWith:      discover.StartWith,
	})
	if errors.Is(err, internalclient.ErrLocalDaemonUnreachable) || errors.Is(err, internalclient.ErrDaemonStartFailed) {
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
	api, err := newGeneratedClient(resolved.BaseURL, merged)
	if err != nil {
		return nil, err
	}
	api.discovered = &DiscoveredDaemon{Remote: resolved.ConfiguredRemote()}
	return api, nil
}
