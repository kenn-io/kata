package client

import (
	"context"
	"errors"
	"fmt"
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
	SourcePath          string
	RuntimeDir          string
	LocalProfile        *LocalProfileIdentity
	profileConfig       *config.LocalProfileConfig
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
	namedRemote     bool
	credentialError error
	persistedAuth   bool
	implicitToken   bool
}

// ConfiguredRemote reports whether resolution selected configured remote
// endpoint metadata rather than local discovery or an injected test target.
func (d ResolvedDaemon) ConfiguredRemote() bool {
	if d.LocalProfile != nil {
		return false
	}
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
		PersistedAuth:    d.persistedAuth,
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
	refreshed.LocalProfile = d.LocalProfile
	refreshed.profileConfig = d.profileConfig
	refreshed.SourcePath = d.SourcePath
	refreshed.RuntimeDir = d.RuntimeDir
	refreshed.credentialError = d.credentialError
	refreshed.implicitToken = d.implicitToken
	if d.implicitToken && !running.PersistedAuth {
		refreshed.Token = ""
		refreshed.implicitToken = false
	}
	return refreshed.withPersistedAuth()
}

func resolvedForRunning(source DaemonSource, name string, running RunningDaemon) ResolvedDaemon {
	resolved := ResolvedDaemon{
		Source:        source,
		Name:          name,
		BaseURL:       running.BaseURL,
		Address:       running.Address,
		Network:       running.Network,
		Scheme:        running.Scheme,
		namedRemote:   source == DaemonSourceNamedCatalog && running.ConfiguredRemote,
		persistedAuth: running.PersistedAuth,
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
	d.credentialError = auth.CredentialError
	d.Token = auth.Token
	if override := authTokenEnvOverride(); override != "" {
		d.Token = override
	}
	d.TrustPrivateNetwork = auth.TrustPrivateNetwork
	return d.withPersistedAuth()
}

func (d ResolvedDaemon) withPersistedAuth() ResolvedDaemon {
	if !d.persistedAuth || d.Token != "" || d.credentialError != nil {
		return d
	}
	var home string
	var err error
	if d.LocalProfile != nil {
		home = d.LocalProfile.Home
	} else if d.Source == DaemonSourceLocalRuntime || (d.Source == DaemonSourceNamedCatalog && !d.namedRemote) {
		home, err = config.KataHome()
	} else {
		return d
	}
	if err == nil {
		d.Token, err = config.ReadPersistedAuthToken(home)
		if err == nil && d.Token == "" {
			err = fmt.Errorf("%w: local runtime's persisted auth token is missing", config.ErrCredentialSource)
		}
	}
	d.credentialError = err
	d.implicitToken = err == nil && d.Token != ""
	return d
}

// GlobalAuthCredential preserves selected-source failures for SDK transports.
// It never discovers the implicit owner token from a local runtime.
func GlobalAuthCredential() (string, error) {
	if override := authTokenEnvOverride(); override != "" {
		return override, nil
	}
	auth := resolveAuthConfig()
	return auth.Token, auth.CredentialError
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
		d.credentialError = auth.CredentialError
	}
	d.TrustPrivateNetwork = auth.TrustPrivateNetwork
	return d.withPersistedAuth()
}

// EnsureResolvedInWorkspace selects a daemon and retains the policy selected
// by that same source. Configured remotes are probed; a local daemon is
// started when it is not running.
func EnsureResolvedInWorkspace(ctx context.Context, workspaceStart string) (ResolvedDaemon, error) {
	return ensureInWorkspace(ctx, workspaceStart, true, false)
}

// EnsureResolvedInWorkspaceForTUI preserves active catalog credentials for
// legacy local entries while retaining the same workspace selection rules.
func EnsureResolvedInWorkspaceForTUI(ctx context.Context, workspaceStart string) (ResolvedDaemon, error) {
	return ensureInWorkspace(ctx, workspaceStart, true, true)
}

// PrepareResolvedInWorkspace selects the target and credentials for an API
// request. Configured remotes are not pinged: the request itself establishes
// reachability. Local discovery, version checks, and auto-start are unchanged.
func PrepareResolvedInWorkspace(ctx context.Context, workspaceStart string) (ResolvedDaemon, error) {
	return ensureInWorkspace(ctx, workspaceStart, false, false)
}

func ensureInWorkspace(
	ctx context.Context, workspaceStart string, probeRemote, keepActiveLocalCatalogCredential bool,
) (ResolvedDaemon, error) {
	if value, ok := ctx.Value(BaseURLKey{}).(string); ok && value != "" {
		return resolvedForRunning(
			DaemonSourceInjected, "", remoteRunningDaemon(value, false),
		).withGlobalAuth(), nil
	}
	selection, err := inspectSelection(ctx, workspaceStart, "", keepActiveLocalCatalogCredential)
	if err != nil {
		return ResolvedDaemon{}, err
	}
	resolved, _, err := bringUpSelection(ctx, selection, workspaceStart, probeRemote, inProcessStart{})
	return resolved, err
}

// ResolveRemoteDaemon resolves only configured remote sources and returns
// their provenance and credentials without falling through to local startup.
func ResolveRemoteDaemon(ctx context.Context, workspaceStart string) (ResolvedDaemon, bool, error) {
	return resolveRemoteDaemon(ctx, workspaceStart)
}

// EnsureResolvedNamed resolves an explicit catalog selection. Named local
// entries may start the local daemon; named remotes are probed.
func EnsureResolvedNamed(ctx context.Context, name string) (ResolvedDaemon, error) {
	return ensureNamed(ctx, name, true)
}

// PrepareResolvedNamed selects a catalog target for an API request without
// probing a remote. Named local daemons retain discovery and auto-start.
func PrepareResolvedNamed(ctx context.Context, name string) (ResolvedDaemon, error) {
	return ensureNamed(ctx, name, false)
}

func ensureNamed(ctx context.Context, name string, probeRemote bool) (ResolvedDaemon, error) {
	if strings.TrimSpace(name) == "" {
		return ResolvedDaemon{}, fmt.Errorf("%w: empty name", ErrNamedDaemonNotFound)
	}
	selection, err := InspectSelection(ctx, "", name)
	if err != nil {
		return ResolvedDaemon{}, err
	}
	resolved, _, err := bringUpSelection(ctx, selection, "", probeRemote, inProcessStart{})
	return resolved, err
}

// DiscoverResolved returns the first live local runtime with its exact
// endpoint and preserves store, cancellation, and unreachable-daemon errors.
func DiscoverResolved(ctx context.Context, dataDir string) (ResolvedDaemon, bool, error) {
	return discoverResolved(ctx, dataDir, liveDaemons)
}

// DiscoverResolvedReadOnly finds a live local runtime without creating or
// repairing runtime metadata. Diagnostics use this to preserve file state.
func DiscoverResolvedReadOnly(ctx context.Context, dataDir string) (ResolvedDaemon, bool, error) {
	return discoverResolved(ctx, dataDir, liveDaemonsReadOnly)
}

func discoverResolved(ctx context.Context, dataDir string, scan liveDaemonScanner) (ResolvedDaemon, bool, error) {
	var unreachable error
	for candidate, err := range scan(ctx, dataDir) {
		if err == nil {
			resolved := localRuntimeResolved(candidate)
			resolved.RuntimeDir = dataDir
			return resolved, true, nil
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
