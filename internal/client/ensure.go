package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

// DiscoveredWebRuntime is the validated browser launch metadata for one live
// daemon record.
type DiscoveredWebRuntime struct {
	Origin       string
	OriginStable bool
	Capabilities []string
}

// DiscoverWebRuntime validates browser metadata from a live runtime record.
func DiscoverWebRuntime(record kitdaemon.RuntimeRecord) (DiscoveredWebRuntime, error) {
	origin := record.Metadata["web_origin"]
	parsedOrigin, err := url.Parse(origin)
	if err != nil || (parsedOrigin.Scheme != "http" && parsedOrigin.Scheme != "https") ||
		parsedOrigin.Host == "" || parsedOrigin.User != nil || parsedOrigin.Path != "" ||
		parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" {
		return DiscoveredWebRuntime{}, errors.New("web runtime origin is invalid")
	}
	stable, err := strconv.ParseBool(record.Metadata["web_origin_stable"])
	if err != nil {
		return DiscoveredWebRuntime{}, errors.New("web runtime origin stability is invalid")
	}
	capabilities := strings.Split(record.Metadata["web_capabilities"], ",")
	if len(capabilities) == 1 && capabilities[0] == "" {
		return DiscoveredWebRuntime{}, errors.New("web runtime capabilities are missing")
	}
	for _, capability := range capabilities {
		if capability == "" || strings.TrimSpace(capability) != capability {
			return DiscoveredWebRuntime{}, errors.New("web runtime capabilities are invalid")
		}
	}

	return DiscoveredWebRuntime{
		Origin:       origin,
		OriginStable: stable,
		Capabilities: capabilities,
	}, nil
}

// BaseURLKey is the context key for injecting a daemon base URL during
// tests, bypassing both Discover and the auto-start path. CLI and TUI
// callers honor it via EnsureRunning.
type BaseURLKey struct{}

// RunningDaemon identifies the endpoint selected by EnsureRunningTarget and
// whether it came from configured remote resolution. Callers that need to
// decide whether client-local filesystem paths are valid must use
// ConfiguredRemote instead of inferring locality from the endpoint address:
// a configured remote may be reached through a loopback SSH tunnel.
type RunningDaemon struct {
	BaseURL          string
	Address          string
	Network          string
	Scheme           string
	ConfiguredRemote bool
}

const (
	daemonServiceName       = "kata"
	skipDaemonVersionEnvVar = "KATA_SKIP_DAEMON_VERSION_CHECK"
	daemonStartupWait       = 30 * time.Second
)

var (
	currentVersionForEnsure      = func() string { return version.Version }
	startDaemonForEnsure         = autoStart
	startDetachedDaemonForEnsure = kitdaemon.StartDetached
	stopRunningDaemonsForEnsure  = stopRunningDaemons
	signalDaemonStopForEnsure    = daemon.SignalDaemonStop
	discoverDaemonForAutoStart   = discoverForEnsure
	checkDaemonStateForEnsure    = func(dataDir string) error {
		return (kitdaemon.RuntimeStore{Dir: dataDir}).CheckWritable()
	}
)

// EnsureRunning returns a live daemon's base URL, auto-starting the daemon
// if no live record is found. Callers that should never spawn a daemon
// (health probes, list commands that should fail loudly) should call
// Discover directly instead.
//
// Test callers can short-circuit discovery by stashing a base URL on ctx
// under BaseURLKey{}.
//
// .kata.local.toml discovery walks upward from CWD. Commands that
// target a specific workspace via --workspace should call
// EnsureRunningInWorkspace instead so the walk anchors to the
// targeted workspace.
func EnsureRunning(ctx context.Context) (string, error) {
	target, err := EnsureRunningTarget(ctx)
	return target.BaseURL, err
}

// EnsureRunningTarget is EnsureRunning with endpoint-source metadata for
// callers that must distinguish configured remotes from local discovery.
func EnsureRunningTarget(ctx context.Context) (RunningDaemon, error) {
	return ensureRunningTargetInWorkspace(ctx, "")
}

// EnsureRunningTargetInWorkspace is the workspace-aware form of
// EnsureRunningTarget. It preserves endpoint-source metadata while anchoring
// .kata.local.toml discovery to workspaceStart.
func EnsureRunningTargetInWorkspace(
	ctx context.Context, workspaceStart string,
) (RunningDaemon, error) {
	return ensureRunningTargetInWorkspace(ctx, workspaceStart)
}

// EnsureRunningInWorkspace is the workspace-aware variant of
// EnsureRunning. workspaceStart is the absolute path to begin the
// .kata.local.toml walk from; pass "" to fall back to CWD. Empty is
// the right value when no --workspace flag is in play; non-empty is
// required so that running `kata --workspace /repo create ...` from
// outside the repo still picks up /repo/.kata.local.toml's [server]
// override instead of falling through to local auto-start.
func EnsureRunningInWorkspace(ctx context.Context, workspaceStart string) (string, error) {
	resolved, err := EnsureResolvedInWorkspace(ctx, workspaceStart)
	return resolved.BaseURL, err
}

func ensureRunningTargetInWorkspace(ctx context.Context, workspaceStart string) (RunningDaemon, error) {
	resolved, err := EnsureResolvedInWorkspace(ctx, workspaceStart)
	return resolved.Running(), err
}

// LocateRunningTargetInWorkspace selects and probes the same endpoint as
// EnsureRunningTargetInWorkspace without resolving bearer credentials. Local
// selections are still started when necessary.
func LocateRunningTargetInWorkspace(ctx context.Context, workspaceStart string) (RunningDaemon, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return remoteRunningDaemon(v, false), nil
	}
	if url, ok, err := resolveRemoteEndpoint(ctx, workspaceStart); err != nil {
		return RunningDaemon{}, err
	} else if ok {
		return remoteRunningDaemon(url, true), nil
	}
	return ensureLocalRunningTarget(ctx)
}

// EnsureLocalRunning returns a live local daemon's base URL, ignoring
// KATA_SERVER and .kata.local.toml remote overrides. Named "local" TUI
// daemon entries use this so selecting local never silently resolves to
// a configured shared daemon.
func EnsureLocalRunning(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return v, nil
	}
	return ensureLocalRunning(ctx)
}

// EnsureLocalRunningTarget is EnsureLocalRunning with the exact selected
// runtime endpoint retained for callers that must publish or reuse it.
func EnsureLocalRunningTarget(ctx context.Context) (RunningDaemon, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return remoteRunningDaemon(v, false), nil
	}
	return ensureLocalRunningTarget(ctx)
}

func ensureLocalRunning(ctx context.Context) (string, error) {
	target, err := ensureLocalRunningTarget(ctx)
	return target.BaseURL, err
}

func ensureLocalRunningTarget(ctx context.Context) (RunningDaemon, error) {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return RunningDaemon{}, err
	}
	found, err := discoverForEnsure(ctx, ns.DataDir)
	if err != nil {
		return RunningDaemon{}, err
	}
	switch found.Outcome {
	case daemonScanCompatible:
		return runningDaemonForLive(found.Daemon), nil
	case daemonScanStale:
		if err := stopRunningDaemonsForEnsure(ctx, ns.DataDir, ns.DBHash); err != nil {
			return RunningDaemon{}, err
		}
	}
	return startDaemonForEnsure(ctx, ns.DataDir)
}

// daemonScanOutcome is the three-case answer to whether a local daemon is
// usable: none answered, one is version-compatible, or one is live but stale.
// The previous (compatible, found) booleans also admitted the meaningless
// compatible-but-not-found combination.
type daemonScanOutcome int

const (
	daemonScanNone daemonScanOutcome = iota
	daemonScanCompatible
	daemonScanStale
)

// ensureDiscovery retains the exact runtime record behind a scan result so
// callers can construct a RunningDaemon without re-deriving its endpoint.
// Daemon is the zero value when Outcome is daemonScanNone.
type ensureDiscovery struct {
	Outcome daemonScanOutcome
	Daemon  liveDaemon
}

func discoverForEnsure(ctx context.Context, dataDir string) (ensureDiscovery, error) {
	var stale ensureDiscovery
	var unreachable error
	for candidate, err := range liveDaemons(ctx, dataDir) {
		if err != nil {
			if errors.Is(err, ErrLocalDaemonUnreachable) {
				if unreachable == nil {
					unreachable = err
				}
				continue
			}
			return ensureDiscovery{}, err
		}
		if daemonVersionCheckSkipped() || daemonVersionCompatible(candidate.Info) {
			return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: candidate}, nil
		}
		if stale.Outcome == daemonScanNone {
			stale = ensureDiscovery{Outcome: daemonScanStale, Daemon: candidate}
		}
	}
	if unreachable != nil {
		return ensureDiscovery{}, unreachable
	}
	return stale, nil
}

func runningDaemonForLive(candidate liveDaemon) RunningDaemon {
	return localRunningDaemon(candidate.BaseURL, candidate.Record.Endpoint().ConfigAddress())
}

func daemonVersionCheckSkipped() bool {
	return os.Getenv(skipDaemonVersionEnvVar) == "1"
}

func daemonVersionCompatible(info PingInfo) bool {
	return info.Service == daemonServiceName && info.Version == currentVersionForEnsure()
}

func stopRunningDaemons(ctx context.Context, dataDir, dbhash string) error {
	signaled := false
	for candidate, err := range liveDaemons(ctx, dataDir) {
		if err != nil {
			if errors.Is(err, ErrLocalDaemonUnreachable) {
				continue
			}
			return err
		}
		if candidate.Info.Service != daemonServiceName || daemonVersionCompatible(candidate.Info) {
			continue
		}
		address := candidate.Record.Endpoint().ConfigAddress()
		if candidate.Info.PID == 0 || candidate.Info.PID != candidate.Record.PID {
			return fmt.Errorf("daemon at %s is running but its PID could not be verified; stop it manually", address)
		}
		if err := signalDaemonStopForEnsure(candidate.Record, dbhash); err != nil {
			return fmt.Errorf("stop old daemon pid %d: %w", candidate.Record.PID, err)
		}
		signaled = true
	}
	if !signaled {
		return nil
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		found, err := discoverForEnsure(ctx, dataDir)
		if err != nil {
			return err
		}
		if found.Outcome == daemonScanNone {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("old daemon did not stop within 3s")
}

func autoStart(ctx context.Context, dataDir string) (RunningDaemon, error) {
	opts := kitdaemon.StartDetachedOptions{
		Args:            []string{"daemon", "start", "--foreground"},
		Env:             append(os.Environ(), daemon.AutoStartMarkerEnv+"=1"),
		RefuseEphemeral: true,
	}
	// The auto-started daemon outlives this process, so it must not inherit
	// our stdio. Inheriting the caller's stderr keeps that handle open after
	// the daemon detaches, which hangs any parent that captures our output
	// (command substitution, CI, pipelines). Send the daemon's stdout/stderr
	// to a daemon.log file under the data dir; if that can't be opened, leave
	// them nil so exec connects the child to the null device. Either way we
	// never hand the daemon the caller's stderr.
	if err := checkDaemonStateForEnsure(dataDir); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return RunningDaemon{}, fmt.Errorf(
				"auto-start daemon: cannot write Kata state directory %s: %w; "+
					"check filesystem permissions or sandbox access and retry",
				dataDir, err,
			)
		}
		return RunningDaemon{}, fmt.Errorf("auto-start daemon: cannot write Kata state directory %s: %w", dataDir, err)
	}
	if logw := daemonLogWriter(dataDir); logw != nil {
		defer func() { _ = logw.Close() }() // child keeps its own handle after Start
		opts.Stdout = logw
		opts.Stderr = logw
	}
	if err := startDetachedDaemonForEnsure(ctx, opts); err != nil {
		return RunningDaemon{}, fmt.Errorf("auto-start daemon: %w", err)
	}
	deadline := time.Now().Add(daemonStartupWait)
	var unreachable error
	for time.Now().Before(deadline) {
		found, err := discoverDaemonForAutoStart(ctx, dataDir)
		if err != nil && !errors.Is(err, ErrLocalDaemonUnreachable) {
			return RunningDaemon{}, err
		}
		if errors.Is(err, ErrLocalDaemonUnreachable) {
			unreachable = err
		}
		if found.Outcome == daemonScanCompatible {
			return runningDaemonForLive(found.Daemon), nil
		}
		select {
		case <-ctx.Done():
			return RunningDaemon{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if unreachable != nil {
		return RunningDaemon{}, unreachable
	}
	return RunningDaemon{}, fmt.Errorf("daemon failed to start within %s; inspect kata daemon status and kata daemon logs", daemonStartupWait)
}

func localRunningDaemon(baseURL, address string) RunningDaemon {
	network := "tcp"
	if strings.HasPrefix(address, "unix://") {
		network = "unix"
	}
	return RunningDaemon{
		BaseURL: baseURL, Address: address, Network: network, Scheme: "http",
	}
}

func remoteRunningDaemon(baseURL string, configured bool) RunningDaemon {
	scheme := "http"
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Scheme != "" {
		scheme = parsed.Scheme
	}
	return RunningDaemon{
		BaseURL: baseURL, Address: baseURL, Network: "tcp", Scheme: scheme,
		ConfiguredRemote: configured,
	}
}

// daemonLogWriter opens <dataDir>/daemon.log for the auto-started daemon's
// stdout+stderr. Returns nil (so exec falls back to the null device) if the
// directory or file cannot be created — the caller must never substitute its
// own stderr, which a detached daemon would hold open and hang the caller.
func daemonLogWriter(dataDir string) *os.File {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil
	}
	//nolint:gosec // G304: dataDir is the daemon's own data dir; filename is the fixed constant "daemon.log".
	f, err := os.OpenFile(filepath.Join(dataDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// DaemonLogWriter opens the shared daemon.log file for daemon subprocess
// stdout/stderr. It returns nil when the log cannot be opened.
func DaemonLogWriter(dataDir string) *os.File {
	return daemonLogWriter(dataDir)
}
