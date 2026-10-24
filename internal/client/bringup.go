package client

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/daemonruntime"
)

// DiscoverRequest selects a daemon the way the CLI does and says how far
// discovery may go to reach it.
type DiscoverRequest struct {
	// WorkspaceStart anchors the upward .kata.local.toml lookup. Empty uses
	// the process working directory.
	WorkspaceStart string
	// Name selects a daemon catalog entry, as --daemon does, and replaces
	// every other selection source.
	Name string
	// ProbeRemote checks that a selected configured remote answers before it
	// is returned.
	ProbeRemote bool
	// ReadOnly never creates or repairs local runtime metadata and leaves a
	// local profile's identity check to the caller's first request.
	ReadOnly bool
}

// DiscoverSelected finds the live daemon a request selects and never starts
// it. A configured remote is returned as selected, probed only when
// ProbeRemote is set. ok is false when the selected local daemon has no live
// runtime; a pinned local profile's selection is still returned so callers can
// diagnose it.
func DiscoverSelected(ctx context.Context, req DiscoverRequest) (ResolvedDaemon, bool, error) {
	selection, err := InspectSelection(ctx, req.WorkspaceStart, req.Name)
	if err != nil {
		return ResolvedDaemon{}, false, err
	}
	var up daemonBringUp = liveDiscovery{scan: liveDaemons, verify: verifyLocalProfileInstance}
	if req.ReadOnly {
		up = liveDiscovery{scan: liveDaemonsReadOnly}
	}
	return bringUpSelection(ctx, selection, req.WorkspaceStart, req.ProbeRemote, up)
}

// daemonBringUp reaches the local daemon a selection names. The adapter
// decides whether reaching it may start it: liveDiscovery never does, and
// inProcessStart starts it from this process.
type daemonBringUp interface {
	profile(context.Context, DaemonSelection) (ResolvedDaemon, bool, error)
	local(context.Context, DaemonSelection) (ResolvedDaemon, bool, error)
}

// bringUpSelection is the one place that decides how each kind of selection
// is reached: a pinned local profile and a local daemon through the adapter,
// a configured remote as selected and never started. It reports a catalog
// credential the selection could not read before reaching anything.
func bringUpSelection(
	ctx context.Context, selection DaemonSelection, workspaceStart string, probe bool, up daemonBringUp,
) (ResolvedDaemon, bool, error) {
	if selection.credentialErr != nil {
		return ResolvedDaemon{}, false, selection.credentialErr
	}
	switch {
	case selection.Profile != nil:
		return up.profile(ctx, selection)
	case selection.Resolved.ConfiguredRemote():
		if probe && !probeRemote(ctx, selection.Resolved.BaseURL) {
			return ResolvedDaemon{}, false, remoteUnavailableError(selection.Resolved, workspaceStart)
		}
		return selection.Resolved, true, nil
	default:
		return up.local(ctx, selection)
	}
}

// selectsCurrentHomeDefault reports a local selection without a catalog
// credential of its own, which uses the default local daemon's auth.
func selectsCurrentHomeDefault(selection DaemonSelection) bool {
	return selection.Resolved.Source == DaemonSourceLocalRuntime && !selection.activeLocalCatalog
}

func remoteUnavailableError(d ResolvedDaemon, workspaceStart string) error {
	source := d.Source.String()
	switch d.Source {
	case DaemonSourceLocalConfig:
		if _, path, ok, err := findLocalConfig(workspaceStart); err == nil && ok {
			source = path
		}
	case DaemonSourceActiveDaemon:
		source = fmt.Sprintf("%s active_daemon %q", daemonConfigSource(), d.Name)
	case DaemonSourceNamedCatalog:
		source = fmt.Sprintf("%s daemon %q", daemonConfigSource(), d.Name)
	}
	return fmt.Errorf("%w: %s (%s)", ErrRemoteUnavailable, d.BaseURL, source)
}

// liveDiscovery finds a running daemon and never starts one.
type liveDiscovery struct {
	scan   liveDaemonScanner
	verify func(context.Context, ResolvedDaemon) error
}

func (d liveDiscovery) profile(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	return discoverLocalProfileWithVerifier(ctx, selection, d.scan, d.verify)
}

func (d liveDiscovery) local(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	namespace, err := daemonruntime.NewNamespace()
	if err != nil {
		return ResolvedDaemon{}, false, err
	}
	found, ok, err := discoverResolved(ctx, namespace.DataDir, d.scan)
	if err != nil || !ok || selectsCurrentHomeDefault(selection) {
		return found, ok, err
	}
	resolved := selection.Resolved.WithRunning(found.Running())
	resolved.UnixSocket = found.UnixSocket
	return resolved, true, nil
}

// inProcessStart starts a stopped local daemon from this process, which is
// the kata binary: it verifies and starts a pinned profile, and starts or
// replaces the current home's daemon.
type inProcessStart struct{}

func (inProcessStart) profile(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	resolved, err := ensureLocalProfile(ctx, selection)
	return resolved, err == nil, err
}

func (inProcessStart) local(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	if selectsCurrentHomeDefault(selection) {
		resolved, err := ensureLocalResolved(ctx)
		return resolved, err == nil, err
	}
	running, err := EnsureLocalRunningTarget(ctx)
	return selection.Resolved.WithRunning(running), err == nil, err
}
