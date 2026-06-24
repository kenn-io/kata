package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
)

// defaultHTTPTimeout is the per-request budget for non-streaming CLI calls.
// Override at runtime with KATA_HTTP_TIMEOUT (any time.ParseDuration string).
const defaultHTTPTimeout = 5 * time.Second

// envHTTPTimeout reads KATA_HTTP_TIMEOUT, falling back to def on empty or
// unparseable input. Bulk imports against an FTS-indexed DB can take longer
// than the default per request, so this knob lets callers extend the budget
// without rebuilding the binary. A non-empty but unparseable value writes a
// warning to stderr — silently using the default would defeat the point of
// setting the env var ("KATA_HTTP_TIMEOUT=30" misses the unit and would
// otherwise look like the bump took effect).
func envHTTPTimeout(def time.Duration) time.Duration {
	v := os.Getenv("KATA_HTTP_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr,
			"kata: ignoring invalid KATA_HTTP_TIMEOUT=%q (expected a Go duration like 30s or 2m); using default %s\n",
			v, def)
		return def
	}
	return d
}

// ensureDaemon discovers a live daemon's HTTP base URL, auto-starting one
// if none is found. Thin wrapper over client.EnsureRunning so the CLI
// and TUI share one resolution path; tests still inject a base URL via
// client.BaseURLKey{} on the context.
//
// When --workspace points at a specific directory, that path anchors
// the .kata.local.toml walk so a workspace-local [server] override is
// honored even when the user is invoking kata from outside the repo.
//
// If a daemon is explicitly configured (via --daemon, KATA_SERVER,
// .kata.local.toml, or active_daemon) but does not respond, the CLI surfaces
// this as a daemon-unavailable error so callers see a stable exit code and shape.
func ensureDaemon(ctx context.Context) (string, error) {
	if flags.Daemon != "" {
		baseURL, err := client.EnsureNamedRunning(ctx, flags.Daemon)
		if err == nil {
			return baseURL, nil
		}
		return "", cliDaemonTargetError(err)
	}
	workspaceStart := workspaceStartForRemote()
	baseURL, err := client.EnsureRunningInWorkspace(ctx, workspaceStart)
	if err == nil {
		return baseURL, nil
	}
	if errors.Is(err, client.ErrRemoteUnavailable) {
		return "", &cliError{
			Message:  err.Error(),
			Kind:     kindDaemonUnavail,
			ExitCode: ExitDaemonUnavail,
		}
	}
	return "", err
}

// workspaceStartForRemote returns the absolute --workspace path when
// the flag is set, or "" to let .kata.local.toml discovery walk from
// CWD. Resolution errors fall through to CWD so a bad --workspace
// surfaces later as a clearer "workspace path" error rather than
// confusing remote-config resolution.
func workspaceStartForRemote() string {
	if flags.Workspace == "" {
		return ""
	}
	abs, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return ""
	}
	return abs
}

// discoverDaemon returns the live daemon URL without auto-starting one.
// Used by health probes and any other surface where "no daemon running"
// is a meaningful answer rather than a state to paper over.
//
// Resolution order matches ensureDaemon so health doesn't disagree
// with the rest of the CLI about which daemon is "the" daemon:
//
//  1. BaseURLKey on the context (test injection).
//  2. --daemon named catalog entry.
//  3. Configured remote (KATA_SERVER env, .kata.local.toml [server].url,
//     or active_daemon). When the remote is set but unreachable,
//     surface that as ErrRemoteUnavailable so health reports the
//     explicitly-selected daemon's actual state rather than silently
//     falling through to a local one.
//  4. Local Discover (runtime files).
//
// Returns a kindDaemonUnavail cliError when no live daemon is found,
// matching hammer-test finding #1's expectation that `kata health`
// doesn't lie about the daemon's actual state.
func discoverDaemon(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(client.BaseURLKey{}).(string); ok && v != "" {
		return v, nil
	}
	if flags.Daemon != "" {
		baseURL, ok, err := client.DiscoverNamed(ctx, flags.Daemon)
		if err == nil && ok {
			return baseURL, nil
		}
		if err == nil {
			return "", noDaemonRunningError()
		}
		return "", cliDaemonTargetError(err)
	}
	if url, ok, err := client.ResolveRemote(ctx, workspaceStartForRemote()); err != nil {
		if errors.Is(err, client.ErrRemoteUnavailable) {
			return "", &cliError{
				Message:  err.Error(),
				Kind:     kindDaemonUnavail,
				ExitCode: ExitDaemonUnavail,
			}
		}
		return "", err
	} else if ok {
		return url, nil
	}
	ns, err := daemon.NewNamespace()
	if err != nil {
		return "", err
	}
	if url, ok := client.Discover(ctx, ns.DataDir); ok {
		return url, nil
	}
	return "", noDaemonRunningError()
}

func noDaemonRunningError() error {
	return &cliError{
		Message:  "no daemon running (start one with `kata daemon start`)",
		Kind:     kindDaemonUnavail,
		ExitCode: ExitDaemonUnavail,
	}
}

func cliDaemonTargetError(err error) error {
	if errors.Is(err, client.ErrNamedDaemonNotFound) {
		return &cliError{
			Message:  err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if errors.Is(err, client.ErrRemoteUnavailable) {
		return &cliError{
			Message:  err.Error(),
			Kind:     kindDaemonUnavail,
			ExitCode: ExitDaemonUnavail,
		}
	}
	return err
}

// httpClientFor returns an *http.Client whose transport understands the
// unix-socket base URL emitted by ensureDaemon. The TUI calls into
// client directly; this wrapper exists only because every existing
// CLI command site is already named for it.
func httpClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	return client.NewHTTPClient(ctx, baseURL,
		client.Opts{
			Timeout:        envHTTPTimeout(defaultHTTPTimeout),
			AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart),
			WorkspaceStart: workspaceStart,
			DaemonName:     flags.Daemon,
		})
}

// longRunningClientFor builds a variant with no overall Client.Timeout for
// commands whose response body is expected to stay open or whose request may
// legitimately take longer than the default CLI request budget.
func longRunningClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	return client.NewHTTPClient(ctx, baseURL, client.Opts{
		AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart),
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
}

// streamingClientFor builds the SSE-friendly variant. Body cancellation comes
// from the request context.
func streamingClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	return client.NewHTTPClient(ctx, baseURL, client.Opts{
		ResponseHeaderTimeout: client.SSEHandshakeTimeout,
		AllowInsecure:         client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart),
		WorkspaceStart:        workspaceStart,
		DaemonName:            flags.Daemon,
	})
}

// resolvedIssueRef captures everything a CLI command needs after parsing a
// user-supplied issue ref: the ref string to send to the daemon ({ref} path
// segment) and the project name the ref binds to. The project name is
// resolved separately into a numeric project ID before building URLs because
// the daemon's path params are still {project_id:int}.
//
// QualifiedID is only populated by callers that need a "<project>#<short_id>"
// display string (e.g. the destructive verbs whose X-Kata-Confirm header
// expects that exact form). It's resolved by the optional daemon lookup
// resolveQualified does, so most commands leave it empty.
type resolvedIssueRef struct {
	// RefForAPI is the literal path component the daemon expects: either a
	// bare short_id ("abc4") or a full 26-char ULID.
	RefForAPI string
	// ProjectName is the project the ref binds to: a qualified ref
	// ("kata#abc4") overrides; a bare short_id / ULID inherits the
	// workspace's project name.
	ProjectName string
	// QualifiedID is "<project_name>#<short_id>" for the resolved issue.
	// Populated by resolveIssueRefForCommandResolved (and its variants),
	// empty otherwise.
	QualifiedID string
	// ShortID is the issue's display short_id after a daemon-side resolve.
	// Populated by the same variants as QualifiedID; empty otherwise.
	ShortID string
}
