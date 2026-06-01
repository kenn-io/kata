package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "manage the kata daemon"}
	cmd.AddCommand(daemonStartCmd(), daemonStatusCmd(), daemonStopCmd(), daemonReloadCmd(), daemonLogsCmd())
	return cmd
}

func daemonStartCmd() *cobra.Command {
	var (
		listen           string
		insecureReadonly bool
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "start the daemon in foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if currentOutputMode() == outputAgent {
				return &cliError{
					Message:  "kata daemon start does not support --agent; run without output formatting",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			return runDaemonWithListen(ctx, listen, insecureReadonly)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "",
		"bind TCP at host:port (admin-only; non-public addresses only). "+
			"Falls back to $KATA_HOME/config.toml's `listen` value when "+
			"unset. Default with neither: Unix socket on Unix; loopback TCP on Windows.")
	cmd.Flags().BoolVar(&insecureReadonly, "insecure-readonly", false,
		"permit unauthenticated GETs on non-loopback TCP when no token "+
			"is configured (DEV ONLY — production must use a token).")
	return cmd
}

func daemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "report whether a daemon is running",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return err
			}
			recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
			if err != nil {
				return err
			}
			out := daemonStatusOutput{Daemons: make([]daemonStatusEntry, 0, len(recs))}
			for _, r := range recs {
				if kitdaemon.ProcessAlive(r.PID) {
					out.Daemons = append(out.Daemons, daemonStatusEntry{
						PID:       r.PID,
						Version:   daemonRuntimeVersion(r),
						Address:   r.Endpoint().ConfigAddress(),
						DBPath:    r.Metadata["db_path"],
						StartedAt: r.StartedAt,
					})
				}
			}
			switch currentOutputMode() {
			case outputAgent:
				status := "stopped"
				if len(out.Daemons) > 0 {
					status = "running"
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK daemon status=%s\n", status)
				return err
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), out)
			}
			if len(out.Daemons) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no daemon running")
				return nil
			}
			for _, d := range out.Daemons {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "daemon pid=%d version=%s address=%s db=%s started_at=%s\n",
					d.PID, d.Version, d.Address, d.DBPath, d.StartedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

type daemonStatusOutput struct {
	Daemons []daemonStatusEntry `json:"daemons"`
}

type daemonStatusEntry struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Address   string    `json:"address"`
	DBPath    string    `json:"db_path"`
	StartedAt time.Time `json:"started_at"`
}

func daemonRuntimeVersion(r kitdaemon.RuntimeRecord) string {
	if r.Version == "" {
		return "unknown"
	}
	return r.Version
}

func daemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "request a graceful shutdown of the running daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return err
			}
			recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
			if err != nil {
				return err
			}
			mode := currentOutputMode()
			pids := make([]int, 0, len(recs))
			for _, r := range recs {
				if !kitdaemon.ProcessAlive(r.PID) {
					continue
				}
				// SignalDaemonStop is platform-specific: SIGTERM on Unix,
				// a named stop event on Windows.
				if err := daemon.SignalDaemonStop(r, ns.DBHash); err != nil {
					return &cliError{
						Kind: kindInternal, ExitCode: ExitInternal,
						Message: fmt.Sprintf("stop pid %d: %v", r.PID, err),
					}
				}
				pids = append(pids, r.PID)
				if mode == outputHuman {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped pid=%d\n", r.PID)
				}
			}
			switch mode {
			case outputAgent:
				switch len(pids) {
				case 0:
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "OK daemon action=stop stopped=0")
				case 1:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon action=stop pid=%d\n", pids[0])
				default:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon action=stop stopped=%d pids=%s\n",
						len(pids), agentValue(joinInts(pids, ",")))
				}
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), daemonStopOutput{
					Action:  "stop",
					Stopped: len(pids),
					PIDs:    pids,
				})
			}
			return nil
		},
	}
}

type daemonStopOutput struct {
	Action  string `json:"action"`
	Stopped int    `json:"stopped"`
	PIDs    []int  `json:"pids"`
}

func joinInts(values []int, sep string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, sep)
}

func daemonReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "ask a running daemon to reload hook config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return err
			}
			recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
			if err != nil {
				return err
			}
			for _, r := range recs {
				if !kitdaemon.ProcessAlive(r.PID) {
					continue
				}
				// SignalDaemonReload is platform-specific: SIGHUP on Unix,
				// a named reload event on Windows.
				if err := daemon.SignalDaemonReload(r, ns.DBHash); err != nil {
					return &cliError{
						Kind: kindInternal, ExitCode: ExitInternal,
						Message: fmt.Sprintf("reload pid %d: %v", r.PID, err),
					}
				}
				switch currentOutputMode() {
				case outputAgent:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon action=reload pid=%d\n", r.PID)
				case outputJSON:
					return emitJSON(cmd.OutOrStdout(), daemonReloadOutput{
						Action: "reload",
						PID:    r.PID,
					})
				default:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"reload signal sent to pid=%d (check daemon log for result)\n", r.PID)
				}
				return nil
			}
			return &cliError{Kind: kindUsage, ExitCode: ExitUsage, Message: "no daemon running"}
		},
	}
}

type daemonReloadOutput struct {
	Action string `json:"action"`
	PID    int    `json:"pid"`
}

// runDaemon is the foreground daemon entry point. Used by `kata daemon start`
// with the platform default endpoint and by the auto-start child process
// spawned by ensureDaemon.
func runDaemon(ctx context.Context) error {
	return runDaemonWithListen(ctx, "", false)
}

// redactRuntimeDSN returns dsn safe for inclusion in the runtime file and
// the `kata daemon status` output. Bare paths and sqlite DSNs pass through
// unchanged via config.RedactDSN; a postgres DSN has its password masked.
// The fallback to dsn handles the (defensive) ambiguous-credentials case
// where config.RedactDSN returns ""; in practice KataDSN validation has
// already rejected the bleed shape upstream, so this branch is unreachable
// through the normal startup path.
func redactRuntimeDSN(dsn string) string {
	if r := config.RedactDSN(dsn); r != "" {
		return r
	}
	return dsn
}

// runDaemonWithListen is the variant used by `kata daemon start --listen`.
// An empty listen string uses the platform default unless
// <KATA_HOME>/config.toml has a `listen = "..."` entry, in which case the
// config value is used. CLI flag always wins over config.
// insecureReadonly is the dev escape hatch from --insecure-readonly.
func runDaemonWithListen(ctx context.Context, listen string, insecureReadonly bool) error {
	dcfg, err := config.ReadDaemonConfig()
	if err != nil {
		return err
	}
	if listen == "" {
		if listen = dcfg.Listen; listen == "" {
			if addr, ok := listenFromPortEnv(); ok {
				listen = addr
			}
		}
	}
	ns, err := daemon.NewNamespace()
	if err != nil {
		return err
	}
	// chooseEndpoint validates the listen shape and address rules (e.g.
	// rejecting literal public IPs like 8.8.8.8) without binding. Run it
	// before the auth-startup guard so a public-address user sees the
	// "non-public" error rather than a generic auth-required message.
	endpoint, err := chooseEndpoint(ns, listen)
	if err != nil {
		return err
	}
	if err := daemon.CheckAuthStartup(listen, dcfg.Auth, insecureReadonly); err != nil {
		return err
	}
	if msg, ok := daemon.TrustPrivateNetworkWarning(listen, dcfg.Auth); ok {
		fmt.Fprintln(os.Stderr, msg)
	}
	if err := ns.EnsureDirs(); err != nil {
		return err
	}

	// Wrap ctx with a local cancel so platform-specific shutdown watchers
	// (e.g. the Windows named-event fired by `kata daemon stop`) can drive
	// a graceful exit. On Unix this is a no-op; SIGTERM delivered to the
	// process by main.go's signal.NotifyContext already cancels ctx.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCleanup := installStopWatcher(ns.DBHash, cancel)
	defer stopCleanup()

	dbPath, err := config.KataDSN(ctx)
	if err != nil {
		return err
	}
	store, err := storeopen.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	disp, daemonLog, hookCfgPath, err := setupHooks(store, dbPath)
	if err != nil {
		return err
	}
	defer shutdownHooks(disp)

	// installReloadSource is platform-specific: SIGHUP delivery on Unix,
	// a named reload event pumped onto the channel on Windows. See
	// daemon_signaling_{unix,windows}.go.
	sigs, reloadCleanup := installReloadSource(ctx, ns.DBHash)
	defer reloadCleanup()
	go runReloadLoop(ctx, sigs, hookCfgPath, disp, daemonLog)
	broadcaster := daemon.NewEventBroadcaster()
	federationWake := startFederationRunner(ctx, store, broadcaster, disp, daemonLog)

	srv := daemon.NewServer(daemon.ServerConfig{
		DB:             store,
		StartedAt:      time.Now().UTC(),
		Endpoint:       &endpoint,
		Hooks:          disp,
		Broadcaster:    broadcaster,
		FederationWake: federationWake,
		CloseThrottle: daemon.CloseThrottlePolicy{
			ThrottleDisabled: !dcfg.Close.Throttle.ThrottleEnabled(),
		},
		Auth:             dcfg.Auth,
		InsecureReadonly: insecureReadonly,
	})
	defer func() { _ = srv.Close() }()

	runtimeStore := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	listener, err := kitdaemon.Listen(ctx, endpoint, kitdaemon.WithRuntimeStore(runtimeStore))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	rec := kitdaemon.NewRuntimeRecord("kata", version.Version, runtimeEndpointForListener(endpoint, listener))
	rec.Metadata = map[string]string{"db_path": redactRuntimeDSN(dbPath)}
	if _, err := runtimeStore.Write(rec); err != nil {
		return err
	}
	runtimeFile := filepath.Join(ns.DataDir, fmt.Sprintf("daemon.%d.json", os.Getpid()))
	defer func() { _ = os.Remove(runtimeFile) }()

	if listen != "" {
		fmt.Fprintf(os.Stderr, "kata daemon: listening on %s\n", rec.Endpoint().ConfigAddress())
	}

	return srv.Serve(ctx, listener)
}

func startFederationRunner(
	ctx context.Context,
	store db.Storage,
	bcast *daemon.EventBroadcaster,
	hookSink hooks.Sink,
	daemonLog *log.Logger,
) func() {
	wake := make(chan struct{}, 1)
	wakeRunner := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	sub := bcast.Subscribe(daemon.SubFilter{})
	go func() {
		defer sub.Unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sub.Ch:
				if !ok {
					return
				}
				if msg.Kind != "event" {
					continue
				}
				wakeRunner()
			}
		}
	}()
	runner := &federation.Runner{
		DB:       store,
		Interval: federationRunnerInterval(),
		Wake:     wake,
		OnError: func(err error) {
			daemonLog.Printf("federation: %v", err)
		},
		OnPulledEvents: func(projectID int64, events []db.Event) {
			for i := range events {
				event := events[i]
				bcast.Broadcast(daemon.StreamMsg{Kind: "event", Event: &event, ProjectID: projectID})
				hookSink.Enqueue(event)
			}
		},
	}
	go func() {
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("federation: %v", err)
		}
	}()
	sweeper := daemon.NewTimedClaimSweeper(store, bcast, hookSink)
	sweeper.OnError = func(err error) {
		daemonLog.Printf("claim sweeper: %v", err)
	}
	go func() {
		if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("claim sweeper: %v", err)
		}
	}()
	return wakeRunner
}

func federationRunnerInterval() time.Duration {
	raw := os.Getenv("KATA_FEDERATION_PULL_INTERVAL_MS")
	if raw == "" {
		return 30 * time.Second
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 30 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// chooseEndpoint picks the daemon's listener: the platform default when
// listen is empty (auto-start path) or a kit TCP endpoint otherwise. We
// pre-flight the address-rule check via ValidateNonPublicAddress so
// the CLI surfaces a clear error before the server starts, without
// the listen-then-close TOCTOU window where the validating bind could
// race with another process or, with port 0, lose the bound port.
// The actual bind happens once in runDaemonWithListen, before the runtime file
// is published, so port 0 can be recorded as its concrete bound port.
func chooseEndpoint(ns *daemon.Namespace, listen string) (kitdaemon.Endpoint, error) {
	if listen == "" {
		return defaultEndpoint(ns), nil
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return kitdaemon.Endpoint{}, fmt.Errorf("kata daemon: invalid --listen value %q: %v", listen, err)
	}
	if err := daemon.ValidateNonPublicAddress(listen); err != nil {
		return kitdaemon.Endpoint{}, fmt.Errorf("kata daemon: invalid --listen value %q: %v", listen, err)
	}
	return kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listen}, nil
}

func defaultEndpoint(ns *daemon.Namespace) kitdaemon.Endpoint {
	return defaultEndpointForOS(ns, runtime.GOOS)
}

func defaultEndpointForOS(ns *daemon.Namespace, goos string) kitdaemon.Endpoint {
	if goos == "windows" {
		return kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:0"}
	}
	socketPath := filepath.Join(ns.SocketDir, "daemon.sock")
	return kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: socketPath}
}

func runtimeEndpointForListener(endpoint kitdaemon.Endpoint, listener net.Listener) kitdaemon.Endpoint {
	if endpoint.Network != "tcp" {
		return endpoint
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listener.Addr().String()}
	}
	if port != "0" {
		return endpoint
	}
	_, boundPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listener.Addr().String()}
	}
	return kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: net.JoinHostPort(host, boundPort)}
}

// listenFromPortEnv reports the bind address to use when the daemon is
// hosted on a PaaS that follows the Heroku-style $PORT contract. Cloud
// Run, Render, Fly.io, Railway, and App Engine all work this way: the
// platform injects PORT into the environment and expects the process to
// bind every interface at 0.0.0.0:$PORT. Consulted only when neither
// --listen nor a config value was supplied.
//
// The auto-start child inherits the parent environment, so a stray PORT
// in a developer's shell would otherwise hijack every implicit daemon
// onto wildcard TCP. We refuse to act on PORT when the auto-start marker
// (daemon.AutoStartMarkerEnv) is set on the process; daemonclient stamps
// it on the child to identify itself.
func listenFromPortEnv() (string, bool) {
	if os.Getenv(daemon.AutoStartMarkerEnv) == "1" {
		return "", false
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort("0.0.0.0", port), true
}

// setupHooks loads hooks.toml, materializes $KATA_HOME, and constructs
// the dispatcher with DB-backed resolvers. Returned values are wired
// into runDaemon: the dispatcher feeds ServerConfig.Hooks, the logger
// is shared with runReloadLoop, and the config path is passed to
// runReloadLoop so SIGHUP re-reads the same file.
func setupHooks(store db.Storage, dbPath string) (*hooks.Dispatcher, *log.Logger, string, error) {
	home, err := config.KataHome()
	if err != nil {
		return nil, nil, "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, "", err
	}
	hookCfgPath, err := config.HookConfigPath()
	if err != nil {
		return nil, nil, "", err
	}
	loaded, err := hooks.LoadStartup(hookCfgPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("hooks: %w", err)
	}
	daemonLog := log.New(os.Stderr, "kata-daemon: ", log.LstdFlags)
	deps := hooks.DispatcherDeps{
		DBHash:          config.DBHash(dbPath),
		KataHome:        home,
		DaemonLog:       daemonLog,
		AliasResolver:   makeAliasResolver(store),
		IssueResolver:   makeIssueResolver(store),
		CommentResolver: makeCommentResolver(store),
		ProjectResolver: makeProjectResolver(store),
		Now:             time.Now,
		GraceWindow:     5 * time.Second,
	}
	disp, err := hooks.New(loaded, deps)
	if err != nil {
		return nil, nil, "", fmt.Errorf("hooks: %w", err)
	}
	return disp, daemonLog, hookCfgPath, nil
}

// shutdownHooks drives the dispatcher's Shutdown with a 10s ceiling.
// Errors (timeout, in-flight jobs) are not returned: the daemon exit
// path proceeds either way, with the dispatcher's own log capturing
// the timeout reason.
func shutdownHooks(disp *hooks.Dispatcher) {
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = disp.Shutdown(sctx)
}
