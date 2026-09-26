package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
)

// defaultHTTPTimeout is the per-request budget for non-streaming CLI calls.
// Override at runtime with KATA_HTTP_TIMEOUT (any time.ParseDuration string).
const defaultHTTPTimeout = client.DefaultHTTPTimeout

// sseHandshakeTimeout bounds response headers on streaming clients; var so
// tests can shorten it.
var sseHandshakeTimeout = client.SSEHandshakeTimeout

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

// ensureDaemonResolved selects a daemon, auto-starting a local one if none is
// found, and carries the source, credentials, and transport policy selected by
// that resolution. Client construction consumes this value without deriving
// policy again from its base URL.
//
// When --workspace points at a specific directory, that path anchors
// the .kata.local.toml walk so a workspace-local [server] override is
// honored even when the user is invoking kata from outside the repo.
//
// Configured remotes are not pinged before ordinary API requests. Discovery
// and health commands use discoverDaemonResolved when liveness is the result.
func ensureDaemonResolved(ctx context.Context) (client.ResolvedDaemon, error) {
	if flags.Daemon != "" {
		resolved, err := client.PrepareResolvedNamed(ctx, flags.Daemon)
		if err != nil {
			return client.ResolvedDaemon{}, cliDaemonTargetError(err)
		}
		return resolved, nil
	}
	workspaceStart := workspaceStartForRemote()
	resolved, err := client.PrepareResolvedInWorkspace(ctx, workspaceStart)
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
	hc, err := client.NewHTTPClient(ctx, baseURL, client.Opts{
		Timeout:        envHTTPTimeout(defaultHTTPTimeout),
		AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart), //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(baseURL, hc)
}

func httpClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	hc, err := client.NewHTTPClientForResolved(ctx, resolved, client.Opts{
		Timeout: envHTTPTimeout(defaultHTTPTimeout),
	})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(resolved.BaseURL, hc)
}

// longRunningClientFor builds a variant with no overall Client.Timeout for
// commands whose response body is expected to stay open or whose request may
// legitimately take longer than the default CLI request budget.
func longRunningClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	hc, err := client.NewHTTPClient(ctx, baseURL, client.Opts{
		AllowInsecure:  client.RemoteAllowInsecureForBaseURL(baseURL, workspaceStart), //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(baseURL, hc)
}

func longRunningClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	hc, err := client.NewHTTPClientForResolved(ctx, resolved, client.Opts{})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(resolved.BaseURL, hc)
}

// streamingClientFor builds the SSE-friendly variant. Body cancellation comes
// from the request context.
func streamingClientFor(ctx context.Context, baseURL string) (*http.Client, error) {
	workspaceStart := workspaceStartForRemote()
	hc, err := client.NewHTTPClient(ctx, baseURL, client.Opts{
		ResponseHeaderTimeout: sseHandshakeTimeout,
		AllowInsecure: client.RemoteAllowInsecureForBaseURL( //nolint:staticcheck // URL-only compatibility caller awaits resolved-target migration.
			baseURL, workspaceStart,
		),
		WorkspaceStart: workspaceStart,
		DaemonName:     flags.Daemon,
	})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(baseURL, hc)
}

func streamingClientForResolved(ctx context.Context, resolved client.ResolvedDaemon) (*http.Client, error) {
	hc, err := client.NewHTTPClientForResolved(ctx, resolved, client.Opts{
		ResponseHeaderTimeout: sseHandshakeTimeout,
	})
	if err != nil {
		return nil, err
	}
	return markDaemonHTTPClient(resolved.BaseURL, hc)
}

type daemonTransportError struct {
	selectedOrigin      string
	unixSocket          bool
	method              string
	phase               string
	possiblyTransmitted bool
	cause               error
}

func (e *daemonTransportError) Error() string {
	subject := "selected daemon"
	if e.unixSocket {
		subject = "local Unix-socket daemon"
	}
	if certificateErr, ok := errors.AsType[*tls.CertificateVerificationError](e.cause); ok {
		return subject + ": " + certificateErr.Error()
	}
	if errors.Is(e.cause, context.Canceled) {
		return subject + " request canceled"
	}
	if e.Timeout() {
		switch e.phase {
		case "connect":
			return subject + " timed out while connecting"
		case "body":
			return subject + " timed out while reading response body"
		default:
			return subject + " timed out while reading response headers"
		}
	}
	switch e.phase {
	case "connect":
		return subject + " is unavailable while connecting"
	case "body":
		return subject + " response was interrupted while reading response body"
	default:
		return subject + " connection closed before response headers arrived"
	}
}

func (e *daemonTransportError) Unwrap() error { return e.cause }

func (e *daemonTransportError) Timeout() bool {
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(e.cause, &netErr) && netErr != nil && netErr.Timeout()
}

func (e *daemonTransportError) Temporary() bool {
	return e.Timeout()
}

func (e *daemonTransportError) daemonPhase() string      { return e.phase }
func (e *daemonTransportError) selectedUnixSocket() bool { return e.unixSocket }

func (e *daemonTransportError) mutationOutcomeUnknown() bool {
	if !e.possiblyTransmitted {
		return false
	}
	switch strings.ToUpper(e.method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

type daemonRequestBudgetKey struct{}

type daemonRequestBudget struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	cancelOnce          sync.Once
	followingRedirect   atomic.Bool
	possiblyTransmitted atomic.Bool
	selectedOrigin      string
	unixSocket          bool
	originalMethod      string
}

func newDaemonRequestBudget(req *http.Request, timeout time.Duration, origin *url.URL) *daemonRequestBudget {
	ctx := req.Context()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout) //nolint:gosec // G118: daemonRequestBudget.stop owns cancellation after the response closes.
	}
	state := &daemonRequestBudget{
		cancel:         cancel,
		selectedOrigin: origin.Scheme + "://" + origin.Host,
		unixSocket:     strings.EqualFold(origin.Hostname(), "kata.invalid"),
		originalMethod: req.Method,
	}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			// Sending can race cancellation once the connection is ready. Keep
			// this conservative boundary across redirects and transport retries.
			state.possiblyTransmitted.Store(true)
		},
	})
	state.ctx = context.WithValue(ctx, daemonRequestBudgetKey{}, state)
	return state
}

func (s *daemonRequestBudget) stop() {
	s.cancelOnce.Do(s.cancel)
}

func daemonBudgetFromContext(ctx context.Context) *daemonRequestBudget {
	state, _ := ctx.Value(daemonRequestBudgetKey{}).(*daemonRequestBudget)
	return state
}

// Only clients built for the selected daemon mark its dial failures. Hub
// clients and requests redirected to other origins retain their own errors.
func markDaemonHTTPClient(baseURL string, hc *http.Client) (*http.Client, error) {
	origin, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	transport := hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	timeout := hc.Timeout
	hc.Timeout = 0
	hc.Transport = daemonErrorTransport{RoundTripper: transport, origin: origin, timeout: timeout}
	checkRedirect := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		var err error
		if checkRedirect != nil {
			err = checkRedirect(req, via)
		} else if len(via) >= 10 {
			err = errors.New("stopped after 10 redirects")
		}
		if err != nil || req.Response == nil || req.Response.Request == nil {
			return err
		}
		state := daemonBudgetFromContext(req.Response.Request.Context())
		if state == nil {
			return nil
		}
		state.followingRedirect.Store(true)
		*req = *req.WithContext(state.ctx)
		return nil
	}
	return hc, nil
}

type daemonErrorTransport struct {
	http.RoundTripper
	origin  *url.URL
	timeout time.Duration
}

func (t daemonErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := daemonBudgetFromContext(req.Context())
	if state == nil {
		state = newDaemonRequestBudget(req, t.timeout, t.origin)
	}
	selected := req.URL.Scheme == t.origin.Scheme && req.URL.Host == t.origin.Host
	request := req.Clone(state.ctx)
	resp, err := t.RoundTripper.RoundTrip(request)
	if resp != nil && resp.Body != nil {
		resp.Body = &daemonResponseBody{ReadCloser: resp.Body, state: state, selected: selected}
	}
	if err == nil || !selected {
		if err != nil {
			state.stop()
		}
		return resp, err
	}
	state.stop()
	phase := "headers"
	possiblyTransmitted := state.possiblyTransmitted.Load()
	if !possiblyTransmitted {
		phase = "connect"
	}
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
		phase = "connect"
	}
	return resp, &daemonTransportError{
		selectedOrigin:      state.selectedOrigin,
		unixSocket:          state.unixSocket,
		method:              state.originalMethod,
		phase:               phase,
		possiblyTransmitted: possiblyTransmitted,
		cause:               err,
	}
}

// Keep the request budget below net/http's Client.Timeout replacement path so
// generated response buffering retains selected-daemon provenance.
type daemonResponseBody struct {
	io.ReadCloser
	state    *daemonRequestBudget
	selected bool
}

func (b *daemonResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == nil {
		return n, nil
	}
	if b.state.followingRedirect.Load() {
		return n, err
	}
	b.state.stop()
	if errors.Is(err, io.EOF) || !b.selected {
		return n, err
	}
	cause := &responseBodyReadError{err: err}
	return n, &daemonTransportError{
		selectedOrigin:      b.state.selectedOrigin,
		unixSocket:          b.state.unixSocket,
		method:              b.state.originalMethod,
		phase:               "body",
		possiblyTransmitted: true,
		cause:               cause,
	}
}

func (b *daemonResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.state.followingRedirect.CompareAndSwap(true, false) {
		return err
	}
	b.state.stop()
	return err
}

// daemonAPI is a resolved connection to one daemon: the base URL, the
// *http.Client configured for it, and the resolution provenance behind both.
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

func emitPassthrough(cmd *cobra.Command, bs []byte) ([]byte, bool, error) {
	if currentOutputMode() != outputJSON {
		return bs, false, nil
	}
	var buf bytes.Buffer
	if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
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
