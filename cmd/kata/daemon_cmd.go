package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/version"
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
			"unset. Default with neither: Unix socket under $KATA_HOME/runtime.")
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
			recs, err := daemon.ListRuntimeFiles(ns.DataDir)
			if err != nil {
				return err
			}
			out := daemonStatusOutput{Daemons: make([]daemonStatusEntry, 0, len(recs))}
			for _, r := range recs {
				if daemon.ProcessAlive(r.PID) {
					out.Daemons = append(out.Daemons, daemonStatusEntry{
						PID:       r.PID,
						Version:   daemonRuntimeVersion(r),
						Address:   r.Address,
						DBPath:    r.DBPath,
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

func daemonRuntimeVersion(r daemon.RuntimeRecord) string {
	if r.Version == "" {
		return "unknown"
	}
	return r.Version
}

func daemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "send SIGTERM to a running daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return err
			}
			recs, err := daemon.ListRuntimeFiles(ns.DataDir)
			if err != nil {
				return err
			}
			mode := currentOutputMode()
			pids := make([]int, 0, len(recs))
			for _, r := range recs {
				if daemon.ProcessAlive(r.PID) {
					p, _ := os.FindProcess(r.PID)
					_ = p.Signal(syscall.SIGTERM)
					pids = append(pids, r.PID)
					if mode == outputHuman {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped pid=%d\n", r.PID)
					}
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
		Short: "send SIGHUP to a running daemon to reload hook config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return err
			}
			recs, err := daemon.ListRuntimeFiles(ns.DataDir)
			if err != nil {
				return err
			}
			for _, r := range recs {
				if !daemon.ProcessAlive(r.PID) {
					continue
				}
				p, err := os.FindProcess(r.PID)
				if err != nil {
					return &cliError{
						Kind: kindInternal, ExitCode: ExitInternal,
						Message: fmt.Sprintf("find pid %d: %v", r.PID, err),
					}
				}
				if err := p.Signal(syscall.SIGHUP); err != nil {
					return &cliError{
						Kind: kindInternal, ExitCode: ExitInternal,
						Message: fmt.Sprintf("signal pid %d: %v", r.PID, err),
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
// (no --listen, default Unix socket) and by the auto-start child process
// spawned by ensureDaemon.
func runDaemon(ctx context.Context) error {
	return runDaemonWithListen(ctx, "", false)
}

// runDaemonWithListen is the variant used by `kata daemon start --listen`.
// An empty listen string preserves the existing Unix-socket path exactly,
// unless <KATA_HOME>/config.toml has a `listen = "..."` entry — in which
// case the config value is used. CLI flag always wins over config.
// insecureReadonly is the dev escape hatch from --insecure-readonly.
func runDaemonWithListen(ctx context.Context, listen string, insecureReadonly bool) error {
	dcfg, err := config.ReadDaemonConfig()
	if err != nil {
		return err
	}
	if listen == "" {
		listen = dcfg.Listen
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
	dbPath, err := config.KataDB()
	if err != nil {
		return err
	}
	if ver, err := db.PeekSchemaVersion(ctx, dbPath); err == nil && ver < db.CurrentSchemaVersion() {
		if err := jsonl.AutoCutover(ctx, dbPath); err != nil {
			return err
		}
	}
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	disp, daemonLog, hookCfgPath, err := setupHooks(store, dbPath)
	if err != nil {
		return err
	}
	defer shutdownHooks(disp)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go runReloadLoop(ctx, sigs, hookCfgPath, disp, daemonLog)

	srv := daemon.NewServer(daemon.ServerConfig{
		DB:        store,
		StartedAt: time.Now().UTC(),
		Endpoint:  endpoint,
		Hooks:     disp,
		CloseThrottle: daemon.CloseThrottlePolicy{
			ThrottleDisabled: !dcfg.Close.Throttle.ThrottleEnabled(),
		},
		Auth:             dcfg.Auth,
		InsecureReadonly: insecureReadonly,
	})
	defer func() { _ = srv.Close() }()

	rec := daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   endpoint.Address(),
		DBPath:    dbPath,
		Version:   version.Version,
		StartedAt: time.Now().UTC(),
	}
	if _, err := daemon.WriteRuntimeFile(ns.DataDir, rec); err != nil {
		return err
	}
	runtimeFile := filepath.Join(ns.DataDir, fmt.Sprintf("daemon.%d.json", os.Getpid()))
	defer func() { _ = os.Remove(runtimeFile) }()

	if listen != "" {
		fmt.Fprintf(os.Stderr, "kata daemon: listening on %s\n", endpoint.Address())
	}

	return srv.Run(ctx)
}

// chooseEndpoint picks the daemon's listener: Unix socket when listen is
// empty (default, auto-start path) or TCPEndpointAny otherwise. We
// pre-flight the address-rule check via ValidateNonPublicAddress so
// the CLI surfaces a clear error before the server starts, without
// the listen-then-close TOCTOU window where the validating bind could
// race with another process or, with port 0, lose the bound port.
// The actual bind happens once inside server.Run.
func chooseEndpoint(ns *daemon.Namespace, listen string) (daemon.DaemonEndpoint, error) {
	if listen == "" {
		socketPath := filepath.Join(ns.SocketDir, "daemon.sock")
		return daemon.UnixEndpoint(socketPath), nil
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return nil, fmt.Errorf("kata daemon: invalid --listen value %q: %v", listen, err)
	}
	if err := daemon.ValidateNonPublicAddress(listen); err != nil {
		return nil, fmt.Errorf("kata daemon: invalid --listen value %q: %v", listen, err)
	}
	return daemon.TCPEndpointAny(listen), nil
}

// setupHooks loads hooks.toml, materializes $KATA_HOME, and constructs
// the dispatcher with DB-backed resolvers. Returned values are wired
// into runDaemon: the dispatcher feeds ServerConfig.Hooks, the logger
// is shared with runReloadLoop, and the config path is passed to
// runReloadLoop so SIGHUP re-reads the same file.
func setupHooks(store *db.DB, dbPath string) (*hooks.Dispatcher, *log.Logger, string, error) {
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
