package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/vector"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "manage the kata daemon"}
	cmd.AddCommand(daemonStartCmd(), daemonStatusCmd(), daemonStopCmd(), daemonRestartCmd(), daemonReloadCmd(), daemonLogsCmd())
	return cmd
}

const daemonTelemetryHeartbeatInterval = 24 * time.Hour

// daemonRestartShutdownTimeout covers the sequential 10-second HTTP and hook
// shutdown budgets, plus a small allowance for the remaining process cleanup.
const daemonRestartShutdownTimeout = 25 * time.Second

var newTelemetryReporter = func(opts telemetry.Options) telemetry.Client {
	return telemetry.NewReporterOrDisabled(opts)
}

type githubSyncDaemonRunner interface {
	Run(context.Context) error
}

var newGitHubSyncDaemonRunner = func(config githubsync.RunnerConfig) githubSyncDaemonRunner {
	return githubsync.NewRunner(config)
}

var newGitHubSyncHTTPFetcher = githubsync.NewHTTPFetcher

func newConfiguredGitHubSyncFetcher(cfg config.GitHubSyncConfig) githubsync.Fetcher {
	return newGitHubSyncHTTPFetcher(githubsync.HTTPFetcherConfig{
		CredentialResolver: githubsync.NewCredentialResolver(cfg, nil),
	})
}

type daemonStartOutput struct {
	Action  string `json:"action"`
	PID     int    `json:"pid"`
	Address string `json:"address"`
	DBPath  string `json:"db_path,omitempty"`
}

var (
	startDetachedDaemon = defaultStartDetachedDaemon
	runDaemonForeground = runDaemonWithListen
)

func daemonStartCmd() *cobra.Command {
	var (
		listen           string
		insecureReadonly bool
		foreground       bool
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "start the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if currentOutputMode() == outputAgent {
				return &cliError{
					Message:  "kata daemon start does not support --agent; run without output formatting",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			if foreground {
				ctx, cancel := context.WithCancel(cmd.Context())
				defer cancel()
				return runDaemonForeground(ctx, listen, insecureReadonly)
			}
			out, err := startDetachedDaemon(cmd.Context(), listen, insecureReadonly)
			if err != nil {
				return err
			}
			if currentOutputMode() == outputJSON {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			switch out.Action {
			case "already_running":
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "daemon already running pid=%d address=%s\n", out.PID, out.Address)
			default:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "started pid=%d address=%s\n", out.PID, out.Address)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false,
		"run the daemon in the current process instead of starting it in the background")
	cmd.Flags().StringVar(&listen, "listen", "",
		"bind TCP at host:port (admin-only; non-public addresses only). "+
			"Falls back to $KATA_HOME/config.toml's `listen` value when "+
			"unset. Default with neither: Unix socket on Unix; loopback TCP on Windows.")
	cmd.Flags().BoolVar(&insecureReadonly, "insecure-readonly", false,
		"permit unauthenticated GETs on non-loopback TCP when no token "+
			"is configured (DEV ONLY — production must use a token).")
	return cmd
}

func defaultStartDetachedDaemon(ctx context.Context, listen string, insecureReadonly bool) (daemonStartOutput, error) {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return daemonStartOutput{}, err
	}
	if err := ns.EnsureDirs(); err != nil {
		return daemonStartOutput{}, err
	}
	effectiveListen, err := effectiveDaemonListen(listen)
	if err != nil {
		return daemonStartOutput{}, err
	}
	if rec, ok := liveDaemonRecord(ns.DataDir, 0); ok {
		address := rec.Endpoint().ConfigAddress()
		if effectiveListen != "" && address != effectiveListen {
			return daemonStartOutput{}, fmt.Errorf("daemon already running at %s; stop it before starting with listener %s", address, effectiveListen)
		}
		return daemonStartOutput{
			Action:  "already_running",
			PID:     rec.PID,
			Address: address,
			DBPath:  rec.Metadata["db_path"],
		}, nil
	}

	args := []string{"daemon", "start", "--foreground"}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	if insecureReadonly {
		args = append(args, "--insecure-readonly")
	}
	opts := kitdaemon.StartDetachedOptions{
		Args:            args,
		Env:             os.Environ(),
		RefuseEphemeral: true,
	}
	if logw := client.DaemonLogWriter(ns.DataDir); logw != nil {
		opts.Stdout = logw
		opts.Stderr = logw
		defer func() { _ = logw.Close() }()
	}
	var childProcess *os.Process
	var pid int
	opts.AfterStart = func(cmd *exec.Cmd) {
		childProcess = cmd.Process
		pid = cmd.Process.Pid
	}
	if err := kitdaemon.StartDetached(ctx, opts); err != nil {
		return daemonStartOutput{}, fmt.Errorf("start daemon: %w", err)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		if rec, ok := liveDaemonRecord(ns.DataDir, pid); ok {
			return daemonStartOutput{
				Action:  "started",
				PID:     rec.PID,
				Address: rec.Endpoint().ConfigAddress(),
				DBPath:  rec.Metadata["db_path"],
			}, nil
		}
		select {
		case <-ctx.Done():
			if childProcess != nil {
				_ = childProcess.Kill()
			}
			return daemonStartOutput{}, ctx.Err()
		case <-deadline.C:
			if childProcess != nil {
				_ = childProcess.Kill()
			}
			return daemonStartOutput{}, daemonStartTimeoutError(ns.DataDir)
		case <-tick.C:
		}
	}
}

func effectiveDaemonListen(listen string) (string, error) {
	if listen != "" {
		return listen, nil
	}
	dcfg, err := config.ReadDaemonConfig()
	if err != nil {
		return "", err
	}
	return effectiveDaemonListenWithConfig(listen, dcfg), nil
}

func effectiveDaemonListenWithConfig(listen string, dcfg *config.DaemonConfig) string {
	if listen != "" {
		return listen
	}
	if dcfg.Listen != "" {
		return dcfg.Listen
	}
	if addr, ok := listenFromPortEnv(); ok {
		return addr
	}
	return ""
}

func liveDaemonRecord(dataDir string, pid int) (kitdaemon.RuntimeRecord, bool) {
	recs, err := (kitdaemon.RuntimeStore{Dir: dataDir}).List()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false
	}
	for _, rec := range recs {
		if pid != 0 && rec.PID != pid {
			continue
		}
		if kitdaemon.ProcessAlive(rec.PID) {
			return rec, true
		}
	}
	return kitdaemon.RuntimeRecord{}, false
}

func daemonStartTimeoutError(dataDir string) error {
	msg := "daemon failed to start within 5s"
	if tail := daemonStartLogTail(dataDir); tail != "" {
		msg = msg + "\n" + tail
	}
	return errors.New(msg)
}

func daemonStartLogTail(dataDir string) string {
	body, err := os.ReadFile(filepath.Join(dataDir, "daemon.log")) //nolint:gosec // G304: dataDir is the daemon namespace, filename is fixed.
	if err != nil {
		return ""
	}
	const maxTail = 4096
	if len(body) > maxTail {
		body = body[len(body)-maxTail:]
	}
	return strings.TrimSpace(string(body))
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
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No kata daemon is running.")
				return nil
			}
			for _, d := range out.Daemons {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "kata running at %s\n", daemonStatusAddress(d.Address))
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  pid:     %d\n", d.PID)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  version: %s\n", d.Version)
				if !d.StartedAt.IsZero() {
					uptime := time.Since(d.StartedAt).Round(time.Second)
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  uptime:  %s\n", uptime)
				}
			}
			return nil
		},
	}
}

func daemonStatusAddress(address string) string {
	if strings.Contains(address, "://") {
		return address
	}
	return "http://" + address
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

func daemonRestartCmd() *cobra.Command {
	var (
		listen           string
		insecureReadonly bool
	)
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "restart the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			startup, err := preflightDaemonStartup(cmd.Context(), listen, insecureReadonly)
			if err != nil {
				return fmt.Errorf("restart: validate replacement: %w", err)
			}
			ns := startup.Namespace
			recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
			if err != nil {
				return err
			}
			pids := make([]int, 0, len(recs))
			for _, rec := range recs {
				if !kitdaemon.ProcessAlive(rec.PID) {
					continue
				}
				if err := daemon.SignalDaemonStop(rec, ns.DBHash); err != nil {
					return fmt.Errorf("restart: stop pid %d: %w", rec.PID, err)
				}
				pids = append(pids, rec.PID)
			}
			if err := waitForDaemonProcesses(cmd.Context(), pids, daemonRestartShutdownTimeout); err != nil {
				return err
			}
			out, err := startDetachedDaemon(cmd.Context(), listen, insecureReadonly)
			if err != nil {
				return err
			}
			switch currentOutputMode() {
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), daemonRestartOutput{
					Action:  "restart",
					Stopped: len(pids),
					PIDs:    pids,
					PID:     out.PID,
					Address: out.Address,
					DBPath:  out.DBPath,
				})
			case outputAgent:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon action=restart pid=%d stopped=%d", out.PID, len(pids))
				if len(pids) > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), " pids=%s", agentValue(joinInts(pids, ",")))
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
			case outputHuman:
				if len(pids) == 0 {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "started pid=%d address=%s (was not running)\n", out.PID, out.Address)
				} else {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "restarted pid=%d address=%s\n", out.PID, out.Address)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "",
		"bind the replacement daemon to host:port (overrides config.toml)")
	cmd.Flags().BoolVar(&insecureReadonly, "insecure-readonly", false,
		"permit unauthenticated GETs on non-loopback TCP when no token is configured (DEV ONLY)")
	return cmd
}

type daemonStartupPreflight struct {
	Config         *config.DaemonConfig
	Listen         string
	Namespace      *daemon.Namespace
	Endpoint       kitdaemon.Endpoint
	DBPath         string
	StoreConfig    storeopen.Config
	KataHome       string
	HookConfigPath string
	HookConfig     hooks.LoadedConfig
	Embedder       *embedding.Client
	VectorsPath    string
}

func preflightDaemonStartup(ctx context.Context, listen string, insecureReadonly bool) (daemonStartupPreflight, error) {
	dcfg, err := config.ReadDaemonConfig()
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	listen = effectiveDaemonListenWithConfig(listen, dcfg)
	ns, err := daemon.NewNamespace()
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	endpoint, err := chooseEndpoint(ns, listen)
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	if err := daemon.CheckAuthStartup(listen, dcfg.Auth, insecureReadonly); err != nil {
		return daemonStartupPreflight{}, err
	}
	dbPath, err := config.KataDSN(ctx)
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	if err := storeopen.Validate(dbPath); err != nil {
		return daemonStartupPreflight{}, err
	}
	storeConfig := storeopen.DefaultConfig()
	backend, err := storeopen.BackendForDSN(dbPath)
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	if backend == storeopen.BackendPostgres {
		storeConfig.Postgres = pgstore.ConfigFromValues(
			dcfg.Storage.Postgres.Schema,
			dcfg.Storage.Postgres.Mode,
			dcfg.Storage.Postgres.SchemaOwner,
			dcfg.Storage.Postgres.AllowInsecure,
		)
		if err := storeConfig.Postgres.Validate(); err != nil {
			return daemonStartupPreflight{}, err
		}
	}
	home, err := config.KataHome()
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	hookCfgPath, err := config.HookConfigPath()
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	loadedHooks, err := hooks.LoadStartup(hookCfgPath)
	if err != nil {
		return daemonStartupPreflight{}, fmt.Errorf("hooks: %w", err)
	}
	embedder, vectorsPath, err := preflightEmbeddingStartup(dcfg.Search.Embeddings, dbPath)
	if err != nil {
		return daemonStartupPreflight{}, err
	}
	return daemonStartupPreflight{
		Config:         dcfg,
		Listen:         listen,
		Namespace:      ns,
		Endpoint:       endpoint,
		DBPath:         dbPath,
		StoreConfig:    storeConfig,
		KataHome:       home,
		HookConfigPath: hookCfgPath,
		HookConfig:     loadedHooks,
		Embedder:       embedder,
		VectorsPath:    vectorsPath,
	}, nil
}

type daemonRestartOutput struct {
	Action  string `json:"action"`
	Stopped int    `json:"stopped"`
	PIDs    []int  `json:"pids"`
	PID     int    `json:"pid"`
	Address string `json:"address"`
	DBPath  string `json:"db_path,omitempty"`
}

func waitForDaemonProcesses(ctx context.Context, pids []int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		allStopped := true
		for _, pid := range pids {
			if kitdaemon.ProcessAlive(pid) {
				allStopped = false
				break
			}
		}
		if allStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("daemon did not stop within %s", timeout)
		case <-tick.C:
		}
	}
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

// runDaemon is the foreground daemon entry point. Used by `kata daemon start
// --foreground` with the platform default endpoint and by the auto-start child
// process spawned by ensureDaemon.
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

// runDaemonWithListen is the variant used by `kata daemon start --foreground --listen`.
// An empty listen string uses the platform default unless
// <KATA_HOME>/config.toml has a `listen = "..."` entry, in which case the
// config value is used. CLI flag always wins over config.
// insecureReadonly is the dev escape hatch from --insecure-readonly.
func runDaemonWithListen(ctx context.Context, listen string, insecureReadonly bool) error {
	startup, err := preflightDaemonStartup(ctx, listen, insecureReadonly)
	if err != nil {
		return err
	}
	dcfg := startup.Config
	listen = startup.Listen
	ns := startup.Namespace
	endpoint := startup.Endpoint
	if msg, ok := daemon.TrustPrivateNetworkWarning(listen, dcfg.Auth); ok {
		fmt.Fprintln(os.Stderr, msg)
	}
	if msg, ok := daemon.UnauthenticatedPrivateNetworkWritesWarning(listen, dcfg.Auth); ok {
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

	dbPath := startup.DBPath
	store, err := storeopen.OpenWithConfig(ctx, dbPath, startup.StoreConfig, db.Serving())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	disp, daemonLog, err := setupHooks(store, ns.DBHash, startup.KataHome, startup.HookConfig)
	if err != nil {
		return err
	}
	defer shutdownHooks(disp)
	hookCfgPath := startup.HookConfigPath

	telemetryReporter := newDaemonTelemetryReporter(store)
	defer func() {
		if err := telemetryReporter.Close(); err != nil {
			daemonLog.Printf("telemetry: close: %v", err)
		}
	}()
	captureDaemonStartedTelemetry(ctx, store, telemetryReporter)
	startDaemonTelemetryHeartbeat(ctx, store, telemetryReporter)

	// installReloadSource is platform-specific: SIGHUP delivery on Unix,
	// a named reload event pumped onto the channel on Windows. See
	// daemon_signaling_{unix,windows}.go.
	sigs, reloadCleanup := installReloadSource(ctx, ns.DBHash)
	defer reloadCleanup()
	go runReloadLoop(ctx, sigs, hookCfgPath, disp, daemonLog)
	broadcaster := daemon.NewEventBroadcaster()
	federationWake := startFederationRunner(ctx, store, broadcaster, disp, daemonLog)
	gitHubSyncFetcher := newConfiguredGitHubSyncFetcher(dcfg.GitHubSync)
	gitHubSyncWake := startGitHubSyncRunner(ctx, store, gitHubSyncFetcher, broadcaster, disp, daemonLog)
	closeThrottleWindow, err := dcfg.Close.Throttle.ThrottleWindow()
	if err != nil {
		return err
	}

	embedder, vectorIndex, reconcilerHealth, err := startEmbeddingReconciler(
		ctx, dcfg.Search.Embeddings, startup.Embedder, startup.VectorsPath, store, broadcaster, daemonLog,
	)
	if err != nil {
		return err
	}
	defer func() {
		if vectorIndex != nil {
			_ = vectorIndex.Close()
		}
	}()

	srv := daemon.NewServer(daemon.ServerConfig{
		DB:                store,
		StartedAt:         time.Now().UTC(),
		Endpoint:          &endpoint,
		Hooks:             disp,
		Broadcaster:       broadcaster,
		FederationWake:    federationWake,
		GitHubSyncFetcher: gitHubSyncFetcher,
		GitHubSyncConfig:  dcfg.GitHubSync,
		GitHubSyncWake:    gitHubSyncWake,
		CloseThrottle: daemon.CloseThrottlePolicy{
			SiblingBurstEnabled: dcfg.Close.Throttle.ThrottleEnabled(),
			SiblingBurstWindow:  closeThrottleWindow,
		},
		Auth:             dcfg.Auth,
		InsecureReadonly: insecureReadonly,
		Embedder:         embedder,
		VectorIndex:      vectorIndex,
		ReconcilerHealth: reconcilerHealth,
	})
	defer func() { _ = srv.Close() }()

	runtimeStore := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	listener, err := kitdaemon.Listen(ctx, endpoint, kitdaemon.WithRuntimeStore(runtimeStore))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	runtimeEndpoint := runtimeEndpointForListener(endpoint, listener)
	rec := kitdaemon.NewRuntimeRecord("kata", version.Version, runtimeEndpoint)
	rec.Address = runtimeEndpoint.ConfigAddress()
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

func newDaemonTelemetryReporter(store db.Storage) telemetry.Client {
	return newTelemetryReporter(telemetry.Options{
		DistinctID: store.InstanceUID(),
		Version:    version.Version,
		Commit:     version.Commit,
	})
}

func captureDaemonStartedTelemetry(ctx context.Context, store db.Storage, reporter telemetry.Client) {
	captureDaemonTelemetryEvent(ctx, store, reporter, "daemon_started")
}

func startDaemonTelemetryHeartbeat(ctx context.Context, store db.Storage, reporter telemetry.Client) {
	if reporter == nil || !reporter.Enabled() {
		return
	}
	go func() {
		runDaemonTelemetryHeartbeat(ctx, func(ctx context.Context) {
			captureDaemonTelemetryEvent(ctx, store, reporter, "daemon_active")
		})
	}()
}

func runDaemonTelemetryHeartbeat(ctx context.Context, capture func(context.Context)) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	capture(ctx)
	timer := time.NewTimer(durationUntilNextUTCDay(time.Now()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			capture(ctx)
			timer.Reset(durationUntilNextUTCDay(time.Now()))
		}
	}
}

func durationUntilNextUTCDay(now time.Time) time.Duration {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(utc)
}

func captureDaemonTelemetryEvent(ctx context.Context, store db.Storage, reporter telemetry.Client, event string) {
	if reporter == nil || !reporter.Enabled() {
		return
	}
	properties := map[string]any{}
	if projects, err := store.ListProjects(ctx); err == nil {
		properties["project_count"] = len(projects)
	}
	if err := reporter.Capture(event, properties); err != nil {
		slog.Warn("capture telemetry event", "err", err)
	}
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

// vectorsPathForDSN places the semantic-search sidecar next to the SQLite
// database file, deriving its name from the database filename
// (/x/kata.db -> /x/kata.vectors.db) so two databases in one directory never
// share sidecar state. The mapping must be injective: a filename ending in
// .db swaps that suffix for .vectors.db, any other filename appends .vectors
// — the outputs can never coincide (one ends in .vectors.db only when its
// input ended in .db), so /x/data and /x/data.db get distinct sidecars.
// SQLite keeps vectors in a derived sidecar. PostgreSQL stores them in its
// canonical pgvector tables and therefore has no sidecar path.
func vectorsPathForDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "", nil
	}
	path := strings.TrimPrefix(dsn, "sqlite://")
	if strings.Contains(path, "://") {
		return "", fmt.Errorf("semantic search requires the sqlite backend, got dsn %s", config.RedactDSN(dsn))
	}
	if stem, ok := strings.CutSuffix(path, ".db"); ok {
		return stem + ".vectors.db", nil
	}
	return path + ".vectors", nil
}

func preflightEmbeddingStartup(
	ec config.EmbeddingsConfig,
	dbPath string,
) (*embedding.Client, string, error) {
	if !ec.Enabled() {
		return nil, "", nil
	}
	embedder, err := embedding.New(embedding.Config{
		BaseURL:             ec.BaseURL,
		Model:               ec.Model,
		APIKey:              ec.ResolvedAPIKey(),
		Salt:                ec.FingerprintSalt,
		Dims:                ec.Dims,
		BatchSize:           ec.BatchSize,
		Timeout:             time.Duration(ec.TimeoutSeconds) * time.Second,
		TrustPrivateNetwork: ec.TrustPrivateNetwork,
	})
	if err != nil {
		return nil, "", fmt.Errorf("embedding client: %w", err)
	}
	vectorsPath, err := vectorsPathForDSN(dbPath)
	if err != nil {
		return nil, "", fmt.Errorf("embedding index: %w", err)
	}
	return embedder, vectorsPath, nil
}

// startEmbeddingReconciler opens the sidecar vector index for the embedding
// client validated during startup preflight, starts the reconciler goroutine,
// subscribes to the broadcaster so new/edited issues are embedded promptly,
// and triggers an initial backfill sweep. It returns the client, the index, and
// a health snapshot func to wire into ServerConfig. When embeddings are not
// configured it returns nils so the daemon behaves exactly as it did before
// semantic search existed. The caller owns the returned *vector.Index's
// lifetime and must close it on shutdown.
func startEmbeddingReconciler(
	ctx context.Context,
	ec config.EmbeddingsConfig,
	embedder *embedding.Client,
	vectorsPath string,
	store db.Storage,
	bcast *daemon.EventBroadcaster,
	daemonLog *log.Logger,
) (*embedding.Client, *vector.Index, func() daemon.ReconcilerHealth, error) {
	if embedder == nil {
		return nil, nil, nil, nil
	}
	var idx *vector.Index
	var err error
	if postgresStore, ok := store.(*pgstore.Store); ok {
		idx, err = vector.OpenPostgres(ctx, postgresStore.DB)
	} else {
		idx, err = vector.Open(ctx, vectorsPath)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedding index: %w", err)
	}
	reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{BatchSize: ec.BatchSize})
	go func() {
		if err := reconciler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("reconciler: %v", err)
		}
	}()
	startEmbeddingNudge(ctx, bcast, reconciler)
	reconciler.Wake() // initial backfill sweep
	return embedder, idx, reconciler.Health, nil
}

// startEmbeddingNudge subscribes to the broadcaster and wakes the reconciler on
// every committed event so new/edited issues are embedded promptly. The
// goroutine exits when ctx is cancelled or the subscription channel closes, and
// always releases the subscription via Unsub.
func startEmbeddingNudge(ctx context.Context, bcast *daemon.EventBroadcaster, r *daemon.Reconciler) {
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
				if msg.Kind == "event" {
					r.Wake()
				}
			}
		}
	}()
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

func startGitHubSyncRunner(
	ctx context.Context,
	store db.Storage,
	fetcher githubsync.Fetcher,
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
	if fetcher == nil {
		fetcher = newConfiguredGitHubSyncFetcher(config.GitHubSyncConfig{})
	}
	if bcast == nil {
		bcast = daemon.NewEventBroadcaster()
	}
	if hookSink == nil {
		hookSink = hooks.NewNoop()
	}
	logger := slog.Default()
	if daemonLog != nil {
		logger = slog.New(slog.NewTextHandler(daemonLog.Writer(), nil))
	}
	runner := newGitHubSyncDaemonRunner(githubsync.RunnerConfig{
		Store:    store,
		Fetcher:  fetcher,
		Logger:   logger,
		Interval: githubSyncRunnerInterval(),
		Wake:     wake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			for i := range events {
				event := events[i]
				bcast.Broadcast(daemon.StreamMsg{Kind: "event", Event: &event, ProjectID: projectID})
				hookSink.Enqueue(event)
			}
			return nil
		},
	})
	go func() {
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if daemonLog != nil {
				daemonLog.Printf("github sync: %v", err)
			} else {
				slog.Warn("github sync", "err", err)
			}
		}
	}()
	return wakeRunner
}

func githubSyncRunnerInterval() time.Duration {
	raw := os.Getenv("KATA_GITHUB_SYNC_INTERVAL_MS")
	if raw == "" {
		return 5 * time.Minute
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 5 * time.Minute
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

// setupHooks materializes $KATA_HOME and constructs the dispatcher from the
// hook configuration parsed during startup preflight.
func setupHooks(
	store db.Storage,
	dbHash string,
	home string,
	loaded hooks.LoadedConfig,
) (*hooks.Dispatcher, *log.Logger, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, err
	}
	daemonLog := log.New(os.Stderr, "kata-daemon: ", log.LstdFlags)
	deps := hooks.DispatcherDeps{
		DBHash:          dbHash,
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
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	return disp, daemonLog, nil
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
