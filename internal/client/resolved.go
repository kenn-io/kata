package client

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/config"
)

// DaemonSource names the input that selected a daemon. The source is carried
// with the endpoint because two inputs can normalize to the same origin while
// supplying different credentials.
type DaemonSource int

// DaemonSource constants identify each supported daemon-selection input.
const (
	DaemonSourceUnknown DaemonSource = iota
	DaemonSourceInjected
	DaemonSourceServerEnv
	DaemonSourceLocalConfig
	DaemonSourceActiveDaemon
	DaemonSourceNamedCatalog
	DaemonSourceLocalRuntime
)

func (s DaemonSource) String() string {
	switch s {
	case DaemonSourceInjected:
		return "injected"
	case DaemonSourceServerEnv:
		return remoteServerEnvVar
	case DaemonSourceLocalConfig:
		return config.LocalConfigFilename
	case DaemonSourceActiveDaemon:
		return "active_daemon"
	case DaemonSourceNamedCatalog:
		return "daemon catalog"
	case DaemonSourceLocalRuntime:
		return "local runtime"
	default:
		return "unknown"
	}
}

// ResolvedDaemon is the complete result of daemon resolution: endpoint
// provenance, exact transport metadata, and the credentials and policy chosen
// for that source. Client construction consumes this value without resolving
// those inputs again.
type ResolvedDaemon struct {
	Source              DaemonSource
	Name                string
	BaseURL             string
	Address             string
	Network             string
	Scheme              string
	UnixSocket          string
	Token               string
	AllowInsecure       bool
	TrustPrivateNetwork bool

	// Named catalog entries can be local or remote while sharing one public
	// source enum. Retain that distinction so Running reproduces the exact
	// RunningDaemon returned by the credential-free named locator.
	namedRemote bool
}

// ConfiguredRemote reports whether resolution selected configured remote
// endpoint metadata rather than local discovery or an injected test target.
func (d ResolvedDaemon) ConfiguredRemote() bool {
	switch d.Source {
	case DaemonSourceServerEnv, DaemonSourceLocalConfig, DaemonSourceActiveDaemon:
		return true
	case DaemonSourceNamedCatalog:
		return d.namedRemote
	default:
		return false
	}
}

// Running returns the credential-free view without dropping exact endpoint
// address, network, or scheme metadata.
func (d ResolvedDaemon) Running() RunningDaemon {
	return RunningDaemon{
		BaseURL:          d.BaseURL,
		Address:          d.Address,
		Network:          d.Network,
		Scheme:           d.Scheme,
		ConfiguredRemote: d.ConfiguredRemote(),
	}
}

// WithRunning replaces only the selected runtime endpoint metadata. It keeps
// the source identity and bearer policy already resolved for the daemon, so a
// local restart can refresh its socket without re-reading credentials.
func (d ResolvedDaemon) WithRunning(running RunningDaemon) ResolvedDaemon {
	refreshed := resolvedForRunning(d.Source, d.Name, running)
	refreshed.Token = d.Token
	refreshed.AllowInsecure = d.AllowInsecure
	refreshed.TrustPrivateNetwork = d.TrustPrivateNetwork
	refreshed.namedRemote = d.namedRemote
	return refreshed
}

func resolvedForRunning(source DaemonSource, name string, running RunningDaemon) ResolvedDaemon {
	resolved := ResolvedDaemon{
		Source:      source,
		Name:        name,
		BaseURL:     running.BaseURL,
		Address:     running.Address,
		Network:     running.Network,
		Scheme:      running.Scheme,
		namedRemote: source == DaemonSourceNamedCatalog && running.ConfiguredRemote,
	}
	if running.Network == "unix" {
		resolved.UnixSocket = running.Address
		if socket, ok := strings.CutPrefix(running.Address, "unix://"); ok {
			resolved.UnixSocket = socket
		}
	}
	return resolved
}

func (d ResolvedDaemon) withGlobalAuth() ResolvedDaemon {
	auth := resolveAuthConfig()
	d.Token = auth.Token
	if override := authTokenEnvOverride(); override != "" {
		d.Token = override
	}
	d.TrustPrivateNetwork = auth.TrustPrivateNetwork
	return d
}

func (d ResolvedDaemon) withRemoteTargetAuth(token string, allowInsecure bool) ResolvedDaemon {
	auth := resolveAuthConfig()
	d.Token = token
	if override := authTokenEnvOverride(); override != "" {
		d.Token = override
	}
	d.AllowInsecure = allowInsecure
	d.TrustPrivateNetwork = auth.TrustPrivateNetwork
	return d
}

func (d ResolvedDaemon) withLocalTargetAuth(token string) ResolvedDaemon {
	auth := resolveAuthConfig()
	d.Token = token
	if d.Token == "" {
		d.Token = auth.Token
	}
	d.TrustPrivateNetwork = auth.TrustPrivateNetwork
	return d
}

// EnsureResolvedInWorkspace selects a daemon and retains the policy selected
// by that same source. Configured remotes precede local discovery and start.
func EnsureResolvedInWorkspace(ctx context.Context, workspaceStart string) (ResolvedDaemon, error) {
	if value, ok := ctx.Value(BaseURLKey{}).(string); ok && value != "" {
		return resolvedForRunning(
			DaemonSourceInjected, "", remoteRunningDaemon(value, false),
		).withGlobalAuth(), nil
	}
	if resolved, ok, err := resolveRemoteDaemon(ctx, workspaceStart); err != nil {
		return ResolvedDaemon{}, err
	} else if ok {
		return resolved, nil
	}
	return ensureLocalResolved(ctx)
}

// ResolveRemoteDaemon resolves only configured remote sources and returns
// their provenance and credentials without falling through to local startup.
func ResolveRemoteDaemon(ctx context.Context, workspaceStart string) (ResolvedDaemon, bool, error) {
	return resolveRemoteDaemon(ctx, workspaceStart)
}

// EnsureResolvedNamed resolves an explicit catalog selection. Named local
// entries may start the local daemon; named remotes are probed.
func EnsureResolvedNamed(ctx context.Context, name string) (ResolvedDaemon, error) {
	return resolveNamedDaemon(ctx, name)
}

// DiscoverResolved returns the first live local runtime with its exact
// endpoint and preserves store, cancellation, and unreachable-daemon errors.
func DiscoverResolved(ctx context.Context, dataDir string) (ResolvedDaemon, bool, error) {
	var unreachable error
	for candidate, err := range liveDaemons(ctx, dataDir) {
		if err == nil {
			return localRuntimeResolved(candidate), true, nil
		}
		if errors.Is(err, ErrLocalDaemonUnreachable) {
			if unreachable == nil {
				unreachable = err
			}
			continue
		}
		return ResolvedDaemon{}, false, err
	}
	return ResolvedDaemon{}, false, unreachable
}

// DiscoverResolvedNamed resolves an explicit catalog selection without
// starting a local daemon. A configured local entry with no live runtime
// returns the zero value and no error, matching DiscoverNamed's not-found
// result while preserving all resolver errors.
func DiscoverResolvedNamed(ctx context.Context, name string) (ResolvedDaemon, error) {
	target, ok, err := discoverNamedDaemonTarget(ctx, name)
	if err != nil {
		return ResolvedDaemon{}, err
	}
	if !ok {
		return ResolvedDaemon{}, nil
	}
	return namedResolved(target), nil
}

func namedResolved(target namedDaemonTarget) ResolvedDaemon {
	running := target.Running
	if running.BaseURL == "" {
		running = remoteRunningDaemon(target.BaseURL, !target.Local)
	}
	resolved := resolvedForRunning(DaemonSourceNamedCatalog, target.Name, running)
	if target.Local {
		return resolved.withLocalTargetAuth(target.Token)
	}
	return resolved.withRemoteTargetAuth(target.Token, target.AllowInsecure)
}

func ensureLocalResolved(ctx context.Context) (ResolvedDaemon, error) {
	running, err := ensureLocalRunningTarget(ctx)
	if err != nil {
		return ResolvedDaemon{}, err
	}
	return resolvedForRunning(DaemonSourceLocalRuntime, "", running).withGlobalAuth(), nil
}

func localRuntimeResolved(candidate liveDaemon) ResolvedDaemon {
	resolved := resolvedForRunning(
		DaemonSourceLocalRuntime, "", runningDaemonForLive(candidate),
	).withGlobalAuth()
	resolved.UnixSocket = candidate.UnixSocket
	return resolved
}
