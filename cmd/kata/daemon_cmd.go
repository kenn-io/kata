package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/activity"
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
	cmd.AddCommand(daemonStartCmd(), daemonStatusCmd(), daemonLocateCmd(), daemonStopCmd(), daemonRestartCmd(), daemonReloadCmd(), daemonLogsCmd())
	return cmd
}

const daemonTelemetryHeartbeatInterval = 24 * time.Hour

const (
	// daemonShutdownDrainTimeout is the shared budget for draining HTTP
	// handlers, daemon workers, hooks, and platform cleanup.
	daemonShutdownDrainTimeout = 25 * time.Second
	// daemonRestartProcessWaitTimeout leaves time after the internal drain
	// deadline for final cleanup and observable process exit.
	daemonRestartProcessWaitTimeout = daemonShutdownDrainTimeout + 5*time.Second
)

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

var openEmbeddingVectorIndex = func(
	ctx context.Context,
	store db.Storage,
	vectorsPath string,
) (*vector.Index, error) {
	if postgresStore, ok := store.(*pgstore.Store); ok {
		return vector.OpenPostgres(ctx, postgresStore.DB)
	}
	return vector.Open(ctx, vectorsPath)
}

func newConfiguredGitHubSyncFetcher(cfg config.GitHubSyncConfig) githubsync.Fetcher {
	return newGitHubSyncHTTPFetcher(githubsync.HTTPFetcherConfig{
		CredentialResolver: githubsync.NewCredentialResolver(cfg, nil),
	})
}

type daemonStartOutput struct {
	Action      string `json:"action"`
	PID         int    `json:"pid"`
	ReplacedPID int    `json:"replaced_pid,omitempty"`
	Address     string `json:"address"`
	DBPath      string `json:"db_path,omitempty"`
	WebURL      string `json:"web_url,omitempty"`
}

type daemonLocateOutput struct {
	Source         string `json:"source"`
	Kind           string `json:"kind"`
	Network        string `json:"network"`
	Scheme         string `json:"scheme"`
	Address        string `json:"address"`
	RequestBaseURL string `json:"request_base_url,omitempty"`
}

func daemonLocateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "locate",
		Short: "print or start the selected daemon endpoint",
		Long: "Print the daemon endpoint selected by Kata, starting it when the selection is local and stopped. Resolution order is " +
			"--daemon, KATA_SERVER, .kata.local.toml [server].url walking up from " +
			"--workspace or the current directory, active_daemon, then the local daemon. " +
			"Local addresses use unix:///path for Unix sockets or host:port for HTTP over TCP; " +
			"configured remotes use their canonical HTTP(S) URL. Output never includes authentication credentials.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := locateDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if currentOutputMode() == outputJSON {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			if currentOutputMode() == outputAgent {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon source=%s kind=%s network=%s scheme=%s address=%s",
					agentValue(out.Source), agentValue(out.Kind), agentValue(out.Network),
					agentValue(out.Scheme), agentValue(out.Address))
				if err == nil && out.RequestBaseURL != "" {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), " request_base_url=%s", agentValue(out.RequestBaseURL))
				}
				if err == nil {
					_, err = fmt.Fprintln(cmd.OutOrStdout())
				}
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Address)
			return err
		},
	}
}

func locateDaemon(ctx context.Context) (daemonLocateOutput, error) {
	var (
		target client.RunningDaemon
		source = "configured"
		err    error
	)
	if flags.Daemon != "" {
		target, err = client.LocateNamedRunningTarget(ctx, flags.Daemon)
		source = "daemon_flag"
	} else {
		target, err = client.LocateRunningTargetInWorkspace(ctx, workspaceStartForRemote())
	}
	if err != nil {
		return daemonLocateOutput{}, cliDaemonTargetError(err)
	}
	kind := "remote"
	if !target.ConfiguredRemote {
		kind = "local"
		if flags.Daemon == "" {
			source = "local_default"
		}
	}
	requestBaseURL := target.BaseURL
	if target.Network == "unix" {
		requestBaseURL = ""
	}
	return daemonLocateOutput{
		Source: source, Kind: kind, Network: target.Network, Scheme: target.Scheme,
		Address: target.Address, RequestBaseURL: requestBaseURL,
	}, nil
}

var (
	startDetachedDaemon  = defaultStartDetachedDaemon
	runDaemonForeground  = runDaemonWithListen
	launchDetachedDaemon = defaultLaunchDetachedDaemon
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
			case "replaced":
				_, err = fmt.Fprintf(cmd.OutOrStdout(),
					"replaced auto-started daemon pid=%d; started pid=%d address=%s\n",
					out.ReplacedPID, out.PID, out.Address)
			default:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "started pid=%d address=%s\n", out.PID, out.Address)
			}
			if err == nil {
				err = writeDaemonWebURL(cmd.OutOrStdout(), out.WebURL)
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
	replacedPID := 0
	if rec, ok := liveDaemonRecord(ns.DataDir, 0); ok {
		address := rec.Endpoint().ConfigAddress()
		if effectiveListen != "" && address != effectiveListen {
			return daemonStartOutput{}, fmt.Errorf("daemon already running at %s; stop it before starting with listener %s", address, effectiveListen)
		}
		// An explicit start means "keep a daemon resident". An auto-started
		// daemon with idle shutdown armed would exit later, so replace it
		// with an explicit process instead of reporting it as running.
		if !daemonRecordAdvertisesIdleShutdown(ctx, rec) {
			return daemonStartOutputFromRecord("already_running", rec), nil
		}
		if err := daemon.SignalDaemonStop(rec, ns.DBHash); err != nil {
			return daemonStartOutput{}, fmt.Errorf("stop auto-started daemon pid %d: %w", rec.PID, err)
		}
		if err := waitForDaemonProcesses(ctx, []int{rec.PID}, daemonRestartProcessWaitTimeout); err != nil {
			return daemonStartOutput{}, fmt.Errorf("stop auto-started daemon pid %d: %w", rec.PID, err)
		}
		replacedPID = rec.PID
	}
	out, err := launchDetachedDaemon(ctx, ns.DataDir, listen, insecureReadonly)
	if err != nil {
		return daemonStartOutput{}, err
	}
	if replacedPID != 0 {
		out.Action = "replaced"
		out.ReplacedPID = replacedPID
	}
	return out, nil
}

// daemonRecordAdvertisesIdleShutdown reports whether the live daemon behind
// rec is an owner-local process that will stop itself when idle. Remote
// records are never idle-eligible, and an unreachable daemon is treated as
// resident so an explicit start does not signal a process it cannot inspect.
func daemonRecordAdvertisesIdleShutdown(ctx context.Context, rec kitdaemon.RuntimeRecord) bool {
	endpoint := rec.Endpoint()
	if !endpoint.IsUnix() {
		host, _, err := net.SplitHostPort(endpoint.Address)
		if err != nil {
			return false
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	httpClient, baseURL := client.LocalHTTPClient(endpoint.ConfigAddress())
	status, body, err := httpDoJSON(ctx, httpClient, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil || status >= http.StatusBadRequest {
		return false
	}
	var health daemonAPIHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return false
	}
	return health.IdleShutdown != nil
}

func defaultLaunchDetachedDaemon(
	ctx context.Context, dataDir, listen string, insecureReadonly bool,
) (daemonStartOutput, error) {
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
	if logw := client.DaemonLogWriter(dataDir); logw != nil {
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
		if rec, ok := liveDaemonRecord(dataDir, pid); ok {
			return daemonStartOutputFromRecord("started", rec), nil
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
			return daemonStartOutput{}, daemonStartTimeoutError(dataDir)
		case <-tick.C:
		}
	}
}

func daemonStartOutputFromRecord(action string, rec kitdaemon.RuntimeRecord) daemonStartOutput {
	return daemonStartOutput{
		Action:  action,
		PID:     rec.PID,
		Address: rec.Endpoint().ConfigAddress(),
		DBPath:  rec.Metadata["db_path"],
		WebURL:  rec.Metadata["web_origin"],
	}
}

func writeDaemonWebURL(w io.Writer, webURL string) error {
	if webURL == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "  web UI:  %s\n", webURL)
	return err
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
		if daemon.RuntimeProcessAlive(rec) {
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
				if daemon.RuntimeProcessAlive(r) {
					out.Daemons = append(out.Daemons, daemonStatusEntry{
						PID:       r.PID,
						Version:   daemonRuntimeVersion(r),
						Address:   r.Endpoint().ConfigAddress(),
						WebURL:    r.Metadata["web_origin"],
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
				webURL := ""
				if len(out.Daemons) == 1 && out.Daemons[0].WebURL != "" {
					webURL = " web_url=" + out.Daemons[0].WebURL
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK daemon status=%s%s\n", status, webURL)
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
				if d.WebURL != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  web UI:  %s\n", d.WebURL)
				}
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
	WebURL    string    `json:"web_url,omitempty"`
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
				if !daemon.RuntimeProcessAlive(r) {
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
				if !daemon.RuntimeProcessAlive(rec) {
					continue
				}
				if err := daemon.SignalDaemonStop(rec, ns.DBHash); err != nil {
					return fmt.Errorf("restart: stop pid %d: %w", rec.PID, err)
				}
				pids = append(pids, rec.PID)
			}
			if err := waitForDaemonProcesses(cmd.Context(), pids, daemonRestartProcessWaitTimeout); err != nil {
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
					WebURL:  out.WebURL,
				})
			case outputAgent:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK daemon action=restart pid=%d stopped=%d", out.PID, len(pids))
				if len(pids) > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), " pids=%s", agentValue(joinInts(pids, ",")))
				}
				if out.WebURL != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), " web_url=%s", agentValue(out.WebURL))
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
			case outputHuman:
				if len(pids) == 0 {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "started pid=%d address=%s (was not running)\n", out.PID, out.Address)
				} else {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "restarted pid=%d address=%s\n", out.PID, out.Address)
				}
				if err == nil {
					err = writeDaemonWebURL(cmd.OutOrStdout(), out.WebURL)
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
	Web            config.WebConfig
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
	if err := validateFederationStartupConfig(dcfg); err != nil {
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
	if dcfg.Web.Listen != "" {
		if err := daemon.CheckWebStartup(dcfg.Web.Listen, dcfg.Auth, insecureReadonly); err != nil {
			return daemonStartupPreflight{}, err
		}
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
		Web:            dcfg.Web,
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
	WebURL  string `json:"web_url,omitempty"`
}

func waitForDaemonProcesses(ctx context.Context, pids []int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		allStopped := !slices.ContainsFunc(pids, kitdaemon.ProcessAlive)
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
				if !daemon.RuntimeProcessAlive(r) {
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

func newAutostartIdleController(
	dcfg config.DaemonConfig,
	autostart bool,
	daemonEndpoint kitdaemon.Endpoint,
	webEndpoint daemon.WebEndpoint,
	onIdle func(timeout time.Duration),
) (*daemon.IdleController, bool, error) {
	timeout, err := dcfg.AutostartIdleTimeoutDuration()
	if err != nil {
		return nil, false, err
	}
	if !autostart || timeout == 0 {
		return nil, false, nil
	}
	if !daemon.AutostartIdleShutdownEligible(daemonEndpoint, webEndpoint, dcfg) {
		return nil, true, nil
	}
	return daemon.NewIdleController(timeout, func() { onIdle(timeout) }), false, nil
}

// announceIdleShutdown records why an auto-started daemon is exiting. The
// detached daemon's stderr is its daemon.log, so this is the only trace of an
// idle exit an operator can find later.
func announceIdleShutdown(w io.Writer, timeout time.Duration) {
	_, _ = fmt.Fprintf(w, "kata daemon: idle shutdown after %s without client activity\n", timeout)
}

// runDaemonWithListen is the variant used by `kata daemon start --foreground --listen`.
// An empty listen string uses the platform default unless
// <KATA_HOME>/config.toml has a `listen = "..."` entry, in which case the
// config value is used. CLI flag always wins over config.
// insecureReadonly is the dev escape hatch from --insecure-readonly.
func runDaemonWithListen(ctx context.Context, listen string, insecureReadonly bool) (returnErr error) {
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
	shutdownTrigger := newDaemonShutdownTrigger(cancel)
	workers := newDaemonWorkerGroup()
	closeDependencies := true
	hooksClosed := false

	dbPath := startup.DBPath
	store, err := storeopen.OpenWithConfig(ctx, dbPath, startup.StoreConfig, db.Serving())
	if err != nil {
		return err
	}
	defer func() {
		if closeDependencies {
			_ = store.Close()
		}
	}()

	runtimeStore := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	listener, err := kitdaemon.Listen(ctx, endpoint, kitdaemon.WithRuntimeStore(runtimeStore))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	runtimeEndpoint := runtimeEndpointForListener(endpoint, listener)
	webEndpoint, err := daemon.ResolveWebEndpoint(daemon.WebEndpointOptions{
		NamespaceID: ns.DBHash,
		GOOS:        runtime.GOOS,
		Daemon:      runtimeEndpoint,
		Config:      startup.Web,
	})
	if err != nil {
		return err
	}
	if webEndpoint.Listener != nil {
		defer func() { _ = webEndpoint.Listener.Close() }()
	}
	idleController, idleIneligible, err := newAutostartIdleController(
		*dcfg,
		os.Getenv(daemon.AutoStartMarkerEnv) == "1",
		runtimeEndpoint,
		webEndpoint,
		func(timeout time.Duration) {
			announceIdleShutdown(os.Stderr, timeout)
			shutdownTrigger.Call()
		},
	)
	if err != nil {
		return err
	}
	if idleIneligible {
		fmt.Fprintln(os.Stderr,
			"kata daemon: auto-start idle shutdown disabled because the daemon is not owner-local")
	}
	var acquireHookDrain hooks.AcquireActivity
	var waitableDrainAdmission activity.WaitableAdmission
	if idleController != nil {
		acquireHookDrain = idleController.DrainAdmission()
		waitableDrainAdmission = idleController.WaitableDrainAdmission()
	}

	disp, daemonLog, err := setupHooks(
		store, ns.DBHash, startup.KataHome, startup.HookConfig, acquireHookDrain,
		withoutEnvironmentKey(os.Environ(), daemon.AutoStartMarkerEnv),
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeDependencies && !hooksClosed {
			fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer fallbackCancel()
			_ = disp.Shutdown(fallbackCtx)
		}
	}()
	hookCfgPath := startup.HookConfigPath

	telemetryReporter := newDaemonTelemetryReporter(store)
	defer func() {
		if !closeDependencies {
			return
		}
		if err := telemetryReporter.Close(); err != nil {
			daemonLog.Printf("telemetry: close: %v", err)
		}
	}()
	closeThrottleWindow, err := dcfg.Close.Throttle.ThrottleWindow()
	if err != nil {
		return err
	}

	allowLocalSession := webEndpoint.AllowsLocalSession(dcfg.Auth)
	allowTrustedProxySession := webEndpoint.AllowsTrustedProxySession(dcfg.Auth)
	updates := "sse"
	if insecureReadonly {
		updates = "poll"
	}
	webAuthentication := webAuthenticationMode(
		insecureReadonly, allowLocalSession, allowTrustedProxySession, dcfg.Auth.Token,
	)
	webCapabilities := []string{webAuthentication, updates}
	webRuntime, err := daemon.NewWebRuntime(daemon.WebRuntimeOptions{
		Origin:       webEndpoint.Origin,
		OriginStable: webEndpoint.OriginStable,
		Capabilities: webCapabilities,
	})
	if err != nil {
		return err
	}
	webSessions, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin:       webEndpoint.Origin,
		OriginStable: webEndpoint.OriginStable,
		InstanceID:   ns.DBHash,
		Writable:     !insecureReadonly,
		Updates:      updates,
		Auth:         dcfg.Auth,
		DB:           store,
	})
	if err != nil {
		return err
	}
	rec := kitdaemon.NewRuntimeRecord("kata", version.Version, runtimeEndpoint)
	rec.Address = runtimeEndpoint.ConfigAddress()
	rec.Metadata = map[string]string{"db_path": redactRuntimeDSN(dbPath)}
	maps.Copy(rec.Metadata, webRuntime.Metadata())

	broadcaster := daemon.NewEventBroadcaster()
	publisher := daemon.NewEventPublisher(broadcaster, disp)
	embedder, vectorIndex, reconcilerHealth, err := startEmbeddingReconciler(
		ctx, workers, waitableDrainAdmission, dcfg.Search.Embeddings, startup.Embedder, startup.VectorsPath, store, broadcaster, daemonLog,
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeDependencies && vectorIndex != nil {
			_ = vectorIndex.Close()
		}
	}()

	captureDaemonStartedTelemetry(ctx, store, telemetryReporter)
	startDaemonTelemetryHeartbeat(ctx, workers, store, telemetryReporter)
	stopCleanup := installStopWatcher(ns.DBHash, shutdownTrigger.Call)
	// installReloadSource is platform-specific: SIGHUP delivery on Unix,
	// a named reload event pumped onto the channel on Windows. See
	// daemon_signaling_{unix,windows}.go.
	sigs, reloadCleanup := installReloadSource(ctx, ns.DBHash)
	workers.Go(func() {
		runReloadLoop(ctx, sigs, hookCfgPath, disp, daemonLog)
	})
	federationWake := startFederationRunner(
		ctx, workers, waitableDrainAdmission, store, publisher, daemonLog,
	)
	federationConfigHealth := startFederationConfigReconciler(
		ctx, workers, waitableDrainAdmission, dcfg, store, federationWake, func(event db.Event, fork activity.Admission) {
			publisher.EventFrom(event.ProjectID, event, fork)
		}, daemonLog,
	)
	gitHubSyncFetcher := newConfiguredGitHubSyncFetcher(dcfg.GitHubSync)
	gitHubSyncWake := startGitHubSyncRunner(
		ctx, workers, waitableDrainAdmission, store, gitHubSyncFetcher, publisher, daemonLog,
	)
	var idleHealth func() daemon.IdleSnapshot
	var idleAdmission daemon.IdleForegroundAdmission
	if idleController != nil {
		idleHealth = idleController.Snapshot
		idleAdmission = idleController
	}
	srv := daemon.NewServer(daemon.ServerConfig{
		DB:                store,
		DefaultTimezone:   dcfg.Timezone,
		StartedAt:         time.Now().UTC(),
		Endpoint:          &endpoint,
		Hooks:             disp,
		Broadcaster:       broadcaster,
		FederationWake:    federationWake,
		FederationCatalog: append([]config.CatalogDaemonConfig(nil), dcfg.Daemons...),
		WebDaemons:        append([]config.CatalogDaemonConfig(nil), dcfg.Daemons...),
		ActiveWebDaemon:   dcfg.ActiveDaemon,
		GitHubSyncFetcher: gitHubSyncFetcher,
		GitHubSyncConfig:  dcfg.GitHubSync,
		GitHubSyncWake:    gitHubSyncWake,
		CloseThrottle: daemon.CloseThrottlePolicy{
			SiblingBurstEnabled: dcfg.Close.Throttle.ThrottleEnabled(),
			SiblingBurstWindow:  closeThrottleWindow,
		},
		Auth:                   dcfg.Auth,
		WebSessions:            webSessions,
		InsecureReadonly:       insecureReadonly,
		Embedder:               embedder,
		VectorIndex:            vectorIndex,
		ReconcilerHealth:       reconcilerHealth,
		FederationConfigHealth: federationConfigHealth,
		IdleAdmission:          idleAdmission,
		IdleShutdownHealth:     idleHealth,
	})
	defer func() { _ = srv.Close() }()
	var stopAdmission func()
	if idleController != nil {
		stopAdmission = idleController.Stop
	}
	shutdown := startDaemonShutdownCoordinator(
		ctx,
		cancel,
		workers,
		disp,
		stopAdmission,
		daemonShutdownDrainTimeout,
		stopCleanup,
		reloadCleanup,
	)
	shutdownTrigger.Set(shutdown.Trigger)
	httpHandlersJoined := true
	defer func() {
		shutdown.HTTPHandlersDone(httpHandlersJoined)
		shutdown.Trigger()
		result := shutdown.Wait()
		if result.SafeToCloseDependencies() {
			hooksClosed = true
		} else {
			closeDependencies = false
		}
		returnErr = errors.Join(returnErr, result.Err())
	}()

	mainPolicy := daemon.ListenerPolicy{Kind: daemon.ListenerSocket}
	bindings := []daemon.ListenerBinding{{Listener: listener, Policy: mainPolicy}}
	if webEndpoint.Shared {
		bindings[0].Policy = daemon.ListenerPolicy{
			Kind:                  daemon.ListenerSharedTCP,
			Origin:                webEndpoint.Origin,
			BackendAuthority:      runtimeEndpoint.Address,
			AllowedHosts:          append([]string(nil), dcfg.Web.AllowedHosts...),
			RequireBrowserSession: true,
			AllowLocalSession:     allowLocalSession,
			WebAuthentication:     webAuthentication,
		}
	} else {
		bindings = append(bindings, daemon.ListenerBinding{
			Listener: webEndpoint.Listener,
			Policy: daemon.ListenerPolicy{
				Kind:                  daemon.ListenerBrowser,
				Origin:                webEndpoint.Origin,
				RequireBrowserSession: true,
				AllowLocalSession:     allowLocalSession,
				WebAuthentication:     webAuthentication,
			},
		})
	}
	var runtimeFile string
	defer func() {
		if runtimeFile != "" {
			_ = os.Remove(runtimeFile)
		}
	}()
	onReady := func() error {
		return shutdown.PublishReady(ctx, func() error {
			path, err := runtimeStore.Write(rec)
			if err != nil {
				return err
			}
			runtimeFile = path
			if idleController != nil {
				idleController.Start()
			}
			if listen != "" {
				fmt.Fprintf(os.Stderr, "kata daemon: listening on %s\n", rec.Endpoint().ConfigAddress())
			}
			return nil
		})
	}
	httpHandlersJoined = false
	serveErr := srv.ServeListenersWithLifecycle(ctx, onReady, shutdown.Trigger, bindings...)
	httpHandlersJoined = !errors.Is(serveErr, daemon.ErrHTTPHandlersUnjoined)
	shutdown.HTTPHandlersDone(httpHandlersJoined)
	shutdown.Trigger()
	if runtimeFile != "" {
		_ = os.Remove(runtimeFile)
	}
	if errors.Is(serveErr, daemon.ErrHTTPHandlersUnjoined) {
		closeDependencies = false
	}
	if errors.Is(serveErr, errDaemonStoppingBeforeReady) {
		return nil
	}
	return serveErr
}

func webAuthenticationMode(
	insecureReadonly bool,
	allowLocalSession bool,
	allowTrustedProxySession bool,
	token string,
) string {
	switch {
	case insecureReadonly && strings.TrimSpace(token) == "":
		return "readonly"
	case allowLocalSession:
		return "loopback"
	case allowTrustedProxySession:
		return "proxy"
	case strings.TrimSpace(token) != "":
		return "login"
	default:
		return "unavailable"
	}
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

func startDaemonTelemetryHeartbeat(
	ctx context.Context,
	workers *daemonWorkerGroup,
	store db.Storage,
	reporter telemetry.Client,
) {
	if reporter == nil || !reporter.Enabled() {
		return
	}
	workers.Go(func() {
		runDaemonTelemetryHeartbeat(ctx, func(ctx context.Context) {
			captureDaemonTelemetryEvent(ctx, store, reporter, "daemon_active")
		})
	})
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
	workers *daemonWorkerGroup,
	drainAdmission activity.WaitableAdmission,
	store db.Storage,
	publisher daemon.EventPublisher,
	daemonLog *log.Logger,
) func() {
	wake := make(chan struct{}, 1)
	wakeRunner := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	sub := publisher.Broadcaster.Subscribe(daemon.SubFilter{})
	workers.Go(func() {
		defer sub.Unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sub.Ch:
				if !ok {
					return
				}
				if msg.Kind != daemon.StreamKindEvent {
					continue
				}
				wakeRunner()
			}
		}
	})
	runner := &federation.Runner{
		DB:             store,
		Interval:       federationRunnerInterval(),
		Wake:           wake,
		DrainAdmission: drainAdmission,
		OnError: func(err error) {
			daemonLog.Printf("federation: %v", err)
		},
		OnPulledEventsFrom: func(projectID int64, events []db.Event, fork activity.Admission) {
			publisher.EventsFrom(projectID, events, fork)
		},
	}
	workers.Go(func() {
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("federation: %v", err)
		}
	})
	sweeper := daemon.NewTimedClaimSweeper(store, publisher)
	sweeper.IdleAdmission = drainAdmission
	sweeper.OnError = func(err error) {
		daemonLog.Printf("claim sweeper: %v", err)
	}
	workers.Go(func() {
		if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("claim sweeper: %v", err)
		}
	})
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
	workers *daemonWorkerGroup,
	drainAdmission activity.WaitableAdmission,
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
	idx, err := openEmbeddingVectorIndex(ctx, store, vectorsPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedding index: %w", err)
	}
	reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{
		BatchSize:      ec.BatchSize,
		DrainAdmission: drainAdmission,
	})
	workers.Go(func() {
		if err := reconciler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			daemonLog.Printf("reconciler: %v", err)
		}
	})
	startEmbeddingNudge(ctx, workers, bcast, reconciler)
	reconciler.Wake() // initial backfill sweep
	return embedder, idx, reconciler.Health, nil
}

// startEmbeddingNudge subscribes to the broadcaster and wakes the reconciler on
// every committed event so new/edited issues are embedded promptly. The
// goroutine exits when ctx is cancelled or the subscription channel closes, and
// always releases the subscription via Unsub.
func startEmbeddingNudge(
	ctx context.Context,
	workers *daemonWorkerGroup,
	bcast *daemon.EventBroadcaster,
	r *daemon.Reconciler,
) {
	sub := bcast.Subscribe(daemon.SubFilter{})
	workers.Go(func() {
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
	})
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
	workers *daemonWorkerGroup,
	drainAdmission activity.WaitableAdmission,
	store db.Storage,
	fetcher githubsync.Fetcher,
	publisher daemon.EventPublisher,
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
	logger := slog.Default()
	if daemonLog != nil {
		logger = slog.New(slog.NewTextHandler(daemonLog.Writer(), nil))
	}
	runner := newGitHubSyncDaemonRunner(githubsync.RunnerConfig{
		Store:          store,
		Fetcher:        fetcher,
		Logger:         logger,
		Interval:       githubSyncRunnerInterval(),
		Wake:           wake,
		DrainAdmission: drainAdmission,
		EventSinkFrom: func(
			_ context.Context,
			projectID int64,
			events []db.Event,
			fork activity.Admission,
		) error {
			publisher.EventsFrom(projectID, events, fork)
			return nil
		},
	})
	workers.Go(func() {
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if daemonLog != nil {
				daemonLog.Printf("github sync: %v", err)
			} else {
				slog.Warn("github sync", "err", err)
			}
		}
	})
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
	acquireDrain hooks.AcquireActivity,
	environment []string,
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
		AcquireDrain:    acquireDrain,
		Environment:     environment,
	}
	disp, err := hooks.New(loaded, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	return disp, daemonLog, nil
}

func withoutEnvironmentKey(environment []string, key string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, hasValue := strings.Cut(entry, "=")
		if hasValue && strings.EqualFold(name, key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
