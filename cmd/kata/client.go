package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
)

// defaultHTTPTimeout is the per-request budget for non-streaming CLI calls.
// Override at runtime with KATA_HTTP_TIMEOUT (any time.ParseDuration string).
const defaultHTTPTimeout = client.DefaultHTTPTimeout

// envHTTPTimeout reads KATA_HTTP_TIMEOUT, falling back to def on empty or
// unparseable input. Bulk imports against an FTS-indexed DB can take longer
// than the default per request, so this knob lets callers extend the budget
// without rebuilding the binary. A non-empty but unparseable value writes a
// warning to stderr — silently using the default would defeat the point of
// setting the env var ("KATA_HTTP_TIMEOUT=30" misses the unit and would
// otherwise look like the bump took effect).
func envHTTPTimeout(def time.Duration) time.Duration {
	v := os.Getenv(client.HTTPTimeoutEnvVar)
	d, err := client.ParseHTTPTimeout(v, def)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"kata: ignoring invalid KATA_HTTP_TIMEOUT=%q (expected a Go duration like 30s or 2m); using default %s\n",
			v, def)
	}
	return d
}

// ensureDaemonResolved discovers a live daemon, auto-starting one if none is
// found, and carries the source, credentials, and transport policy selected by
// that resolution. Client construction consumes this value without deriving
// policy again from its base URL.
//
// When --workspace points at a specific directory, that path anchors
// the .kata.local.toml walk so a workspace-local [server] override is
// honored even when the user is invoking kata from outside the repo.
//
// If a daemon is explicitly configured (via --daemon, KATA_SERVER,
// .kata.local.toml, or active_daemon) but does not respond, the CLI surfaces
// this as a daemon-unavailable error so callers see a stable exit code and shape.
func ensureDaemonResolved(ctx context.Context) (client.ResolvedDaemon, error) {
	if flags.Daemon != "" {
		resolved, err := client.EnsureResolvedNamed(ctx, flags.Daemon)
		if err != nil {
			return client.ResolvedDaemon{}, cliDaemonTargetError(err)
		}
		return resolved, nil
	}
	workspaceStart := workspaceStartForRemote()
	resolved, err := client.EnsureResolvedInWorkspace(ctx, workspaceStart)
	if err != nil {
		return client.ResolvedDaemon{}, cliDaemonTargetError(err)
	}
	return resolved, nil
}

// ensureDaemon is the URL-only compatibility view used by command paths that
// have not moved to the resolved client constructors yet.
func ensureDaemon(ctx context.Context) (string, error) {
	resolved, err := ensureDaemonResolved(ctx)
	return resolved.BaseURL, err
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
func discoverDaemonResolved(ctx context.Context) (client.ResolvedDaemon, error) {
	if v, ok := ctx.Value(client.BaseURLKey{}).(string); ok && v != "" {
		// The injected branch returns before remote resolution or local startup,
		// while retaining the same global auth policy as ensured injection.
		return client.EnsureResolvedInWorkspace(ctx, "")
	}
	if flags.Daemon != "" {
		resolved, err := client.DiscoverResolvedNamed(ctx, flags.Daemon)
		if err != nil {
			return client.ResolvedDaemon{}, cliDaemonTargetError(err)
		}
		if resolved.BaseURL == "" {
			return client.ResolvedDaemon{}, noDaemonRunningError()
		}
		return resolved, nil
	}
	if resolved, ok, err := client.ResolveRemoteDaemon(ctx, workspaceStartForRemote()); err != nil {
		if errors.Is(err, client.ErrRemoteUnavailable) {
			return client.ResolvedDaemon{}, &cliError{
				Message:  err.Error(),
				Kind:     kindDaemonUnavail,
				ExitCode: ExitDaemonUnavail,
			}
		}
		return client.ResolvedDaemon{}, err
	} else if ok {
		return resolved, nil
	}
	ns, err := daemon.NewNamespace()
	if err != nil {
		return client.ResolvedDaemon{}, err
	}
	if resolved, ok, err := client.DiscoverResolved(ctx, ns.DataDir); err != nil {
		return client.ResolvedDaemon{}, cliDaemonTargetError(err)
	} else if ok {
		return resolved, nil
	}
	return client.ResolvedDaemon{}, noDaemonRunningError()
}

// discoverDaemon is the URL-only compatibility view used by command paths
// that have not moved to the resolved client constructors yet.
func discoverDaemon(ctx context.Context) (string, error) {
	resolved, err := discoverDaemonResolved(ctx)
	return resolved.BaseURL, err
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
	if errors.Is(err, client.ErrRemoteUnavailable) ||
		errors.Is(err, client.ErrLocalDaemonUnreachable) {
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
	return client.NewHTTPClient(ctx, baseURL, client.Opts{
		Timeout:        envHTTPTimeout(defaultHTTPTimeout),
		AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart), //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
}

func httpClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	return client.NewHTTPClientForResolved(ctx, resolved, client.Opts{
		Timeout: envHTTPTimeout(defaultHTTPTimeout),
	})
}

// longRunningClientFor builds a variant with no overall Client.Timeout for
// commands whose response body is expected to stay open or whose request may
// legitimately take longer than the default CLI request budget.
func longRunningClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	return client.NewHTTPClient(ctx, baseURL, client.Opts{
		AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart), //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
}

func longRunningClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	return client.NewHTTPClientForResolved(ctx, resolved, client.Opts{})
}

// streamingClientFor builds the SSE-friendly variant. Body cancellation comes
// from the request context.
func streamingClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	return client.NewHTTPClient(ctx, baseURL, client.Opts{
		ResponseHeaderTimeout: client.SSEHandshakeTimeout,
		AllowInsecure: client.RemoteAllowInsecureForBaseURL( //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
			baseURL, workspaceStart,
		),
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
}

func streamingClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	return client.NewHTTPClientForResolved(ctx, resolved, client.Opts{
		ResponseHeaderTimeout: client.SSEHandshakeTimeout,
	})
}

// daemonAPI is a resolved connection to one daemon: the base URL, the
// *http.Client configured for it, and the resolution provenance behind both.
// Paths passed to its methods are API-relative ("/api/v1/…").
//
// The ctx field is deliberate: a daemonAPI is built inside a command's RunE
// and discarded when it returns, so it carries that invocation's context
// rather than making every call site thread it. Do not store one in a
// longer-lived struct or share it across goroutines.
//
// dialDaemon is LOCAL-DAEMON-ONLY. A federation hub is reached through hubAPI
// with a client built by the hub-specific constructors; the local daemon's
// token must never travel to a hub.
type daemonAPI struct {
	ctx      context.Context
	baseURL  string
	client   *http.Client
	resolved client.ResolvedDaemon
}

// dialDaemon resolves the local daemon (auto-starting one if needed) and
// builds the standard request-budget client for it.
func dialDaemon(ctx context.Context) (daemonAPI, error) {
	return dialResolved(ctx, httpClientForResolved)
}

func dialResolved(
	ctx context.Context,
	build func(context.Context, client.ResolvedDaemon) (*http.Client, error),
) (daemonAPI, error) {
	resolved, err := ensureDaemonResolved(ctx)
	if err != nil {
		return daemonAPI{}, err
	}
	hc, err := build(ctx, resolved)
	if err != nil {
		return daemonAPI{}, err
	}
	return daemonAPI{ctx: ctx, baseURL: resolved.BaseURL, client: hc, resolved: resolved}, nil
}

// discoverDaemonAPI is dialDaemon without auto-start, for probes where "no
// daemon running" is a meaningful answer.
func discoverDaemonAPI(ctx context.Context) (daemonAPI, error) {
	resolved, err := discoverDaemonResolved(ctx)
	if err != nil {
		return daemonAPI{}, err
	}
	hc, err := httpClientForResolved(ctx, resolved)
	if err != nil {
		return daemonAPI{}, err
	}
	return daemonAPI{ctx: ctx, baseURL: resolved.BaseURL, client: hc, resolved: resolved}, nil
}

// hubAPI wraps an already-constructed federation hub client. It is a separate
// constructor on purpose: hub credentials are resolved by the hub-specific
// paths (resolveHubAdminAuth / federationEnrollHTTPClient) and must never come
// from local daemon resolution.
func hubAPI(ctx context.Context, hubBaseURL string, hc *http.Client) daemonAPI {
	return daemonAPI{ctx: ctx, baseURL: strings.TrimRight(hubBaseURL, "/"), client: hc}
}

func (a daemonAPI) url(path string) string {
	return strings.TrimRight(a.baseURL, "/") + path
}

// status is the raw form: the HTTP status and body with no error mapping. Use
// it only where a call site deliberately diverges from the >= 400 rule.
func (a daemonAPI) status(method, path string, body any) (int, []byte, error) {
	return httpDoJSON(a.ctx, a.client, method, a.url(path), body)
}

// do performs the request and maps any status >= 400 to a cliError.
func (a daemonAPI) do(method, path string, body any) ([]byte, error) {
	status, bs, err := a.status(method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, apiErrFromBody(status, bs)
	}
	return bs, nil
}

func (a daemonAPI) doWithHeaders(method, path string, headers map[string]string, body any) ([]byte, error) {
	status, bs, err := httpDoJSONWithHeader(a.ctx, a.client, method, a.url(path), headers, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, apiErrFromBody(status, bs)
	}
	return bs, nil
}

// decode performs the request and unmarshals a successful body into out.
func (a daemonAPI) decode(method, path string, body, out any) error {
	bs, err := a.do(method, path, body)
	if err != nil {
		return err
	}
	return json.Unmarshal(bs, out)
}

// passthrough performs the request and, in JSON output mode, writes the
// daemon's response body straight through. The bool reports whether output was
// written, so callers can return immediately in JSON mode and fall through to
// their human/agent rendering otherwise.
func (a daemonAPI) passthrough(cmd *cobra.Command, method, path string, body any) ([]byte, bool, error) {
	bs, err := a.do(method, path, body)
	if err != nil {
		return nil, false, err
	}
	if currentOutputMode() != outputJSON {
		return bs, false, nil
	}
	var buf bytes.Buffer
	if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
		return nil, false, err
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), buf.String()); err != nil {
		return nil, false, err
	}
	return bs, true, nil
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
