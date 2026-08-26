// Package client resolves a running kata daemon and builds matching
// *http.Clients for both unix-socket and tcp endpoints. Both the kata CLI
// (cmd/kata) and the kata TUI (internal/tui) consume this so the discovery
// rules — runtime-file scan, alive-pid filter, /ping handshake, magic
// http://kata.invalid base URL for unix transport — stay in one place.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

// UnixBase is the synthetic base URL used when the daemon listens on a Unix
// socket. NewHTTPClient/NewStreamingClient detect this prefix and route
// requests through a unix-socket transport instead of TCP DNS.
const UnixBase = "http://kata.invalid"

const (
	// HTTPTimeoutEnvVar configures non-streaming request and configured-remote
	// probe budgets.
	HTTPTimeoutEnvVar = "KATA_HTTP_TIMEOUT"

	// DefaultHTTPTimeout is the normal non-streaming request and
	// configured-remote probe budget.
	DefaultHTTPTimeout = 5 * time.Second
)

// ParseHTTPTimeout parses a positive Go duration, returning fallback for an
// empty or invalid value. Invalid non-empty values also return an error so
// interactive callers can decide whether and where to warn.
func ParseHTTPTimeout(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback, fmt.Errorf("parse HTTP timeout %q: %w", raw, err)
	}
	if d <= 0 {
		return fallback, fmt.Errorf("HTTP timeout must be positive, got %q", raw)
	}
	return d, nil
}

// PingInfo is the live daemon identity returned by /api/v1/ping.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid,omitempty"`
}

// ErrLocalDaemonUnreachable identifies a live local daemon process whose
// recorded endpoint could not be reached.
var ErrLocalDaemonUnreachable = errors.New("local daemon is unreachable")

type localDaemonUnreachableError struct {
	pid     int
	address string
	cause   error
}

func (e *localDaemonUnreachableError) Error() string {
	return fmt.Sprintf("daemon pid %d is running at %s but is unreachable: %v",
		e.pid, e.address, e.cause)
}

func (e *localDaemonUnreachableError) Unwrap() error {
	return e.cause
}

func (e *localDaemonUnreachableError) Is(target error) bool {
	return target == ErrLocalDaemonUnreachable
}

// liveDaemon is one runtime record that answered /api/v1/ping: the record
// itself, the URL callers should dial, the socket path for unix endpoints,
// and the identity the daemon reported. It is the single definition of "a
// daemon worth talking to" so discovery call sites can filter one scan.
type liveDaemon struct {
	Record     kitdaemon.RuntimeRecord
	BaseURL    string
	UnixSocket string
	Info       PingInfo
}

// liveDaemons scans the namespace's runtime records lazily. A successful
// result contains a live daemon; an error result preserves store, context, or
// unreachable-endpoint failures for callers that must distinguish them from
// an empty store. Laziness is load bearing: Discover runs on every CLI
// invocation and must stop after the first live record.
func liveDaemons(ctx context.Context, dataDir string) iter.Seq2[liveDaemon, error] {
	return func(yield func(liveDaemon, error) bool) {
		recs, err := (kitdaemon.RuntimeStore{Dir: dataDir}).List()
		if err != nil {
			yield(liveDaemon{}, err)
			return
		}
		for _, r := range recs {
			if !daemon.RuntimeProcessAlive(r) {
				continue
			}
			ep := r.Endpoint()
			address := ep.ConfigAddress()
			url, info, probeErr := probeAddressWithError(ctx, address)
			if probeErr != nil {
				if err := ctx.Err(); err != nil {
					yield(liveDaemon{}, err)
					return
				}
				if !yield(liveDaemon{}, &localDaemonUnreachableError{
					pid:     r.PID,
					address: address,
					cause:   probeErr,
				}) {
					return
				}
				continue
			}
			candidate := liveDaemon{Record: r, BaseURL: url, Info: info}
			if ep.IsUnix() {
				candidate.UnixSocket = ep.Address
			}
			if !yield(candidate, nil) {
				return
			}
		}
	}
}

// Discover scans the namespace's runtime files and returns the base URL of
// the first daemon that passes /api/v1/ping. The bool is false when no live
// runtime record exists. A live process whose endpoint cannot be reached
// returns ErrLocalDaemonUnreachable so callers do not mistake a permission or
// transport failure for an absent daemon.
func Discover(ctx context.Context, dataDir string) (string, bool, error) {
	var unreachable error
	for candidate, err := range liveDaemons(ctx, dataDir) {
		if err == nil {
			return candidate.BaseURL, true, nil
		}
		if errors.Is(err, ErrLocalDaemonUnreachable) {
			if unreachable == nil {
				unreachable = err
			}
			continue
		}
		return "", false, err
	}
	return "", false, unreachable
}

func probeAddress(ctx context.Context, address string) (string, PingInfo, bool) {
	url, info, err := probeAddressWithError(ctx, address)
	return url, info, err == nil
}

func probeAddressWithError(ctx context.Context, address string) (string, PingInfo, error) {
	client, base := LocalHTTPClient(address)
	info, err := Probe(ctx, client, base)
	if err != nil {
		return "", PingInfo{}, err
	}
	return base, info, nil
}

// LocalHTTPClient returns a short-timeout client and request base for a local
// daemon runtime address: a `unix://` socket path or a plain host:port.
func LocalHTTPClient(address string) (*http.Client, string) {
	if path, ok := strings.CutPrefix(address, "unix://"); ok {
		return &http.Client{Transport: UnixTransport(path), Timeout: 1 * time.Second}, UnixBase
	}
	return &http.Client{Timeout: 1 * time.Second}, "http://" + address
}

// Ping is true when GET base+/api/v1/ping returns 200.
func Ping(ctx context.Context, client *http.Client, base string) bool {
	_, err := Probe(ctx, client, base)
	return err == nil
}

// Probe returns the daemon identity from GET base+/api/v1/ping.
func Probe(ctx context.Context, client *http.Client, base string) (PingInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/ping", nil) //nolint:gosec // G704: base built from our own runtime file
	if err != nil {
		return PingInfo{}, err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: base built from our own runtime file
	if err != nil {
		return PingInfo{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PingInfo{}, fmt.Errorf("daemon ping returned %d", resp.StatusCode)
	}
	var info PingInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return PingInfo{}, fmt.Errorf("decode daemon ping: %w", err)
	}
	if !info.OK {
		return PingInfo{}, errors.New("daemon ping returned ok=false")
	}
	return info, nil
}

// UnixTransport builds a *http.Transport whose DialContext talks to the
// named Unix socket. Used by both the discovery probe and NewHTTPClient.
func UnixTransport(path string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
}

// SSEHandshakeTimeout caps how long NewStreamingClient waits for response
// headers. Wired onto the transport so SSE body reads stay unbounded; only
// a stalled handshake is bounded.
const SSEHandshakeTimeout = 10 * time.Second

// Opts shapes both NewHTTPClient and NewStreamingClient. ResponseHeaderTimeout
// is non-zero only for SSE clients.
type Opts struct {
	Timeout               time.Duration
	ResponseHeaderTimeout time.Duration
	AllowInsecure         bool
	// WorkspaceStart is retained for URL-only compatibility callers that have
	// not yet migrated to ResolvedDaemon. It scopes the temporary active-daemon
	// credential lookup and must not be used by resolved construction.
	// Deprecated: resolve the daemon and call NewHTTPClientForResolved.
	WorkspaceStart string
	// DaemonName is retained for URL-only compatibility callers that discard a
	// named daemon's provenance before client construction.
	// Deprecated: resolve the daemon and call NewHTTPClientForResolved.
	DaemonName string
}

// TargetAuth is explicit per-target bearer configuration. It is used by
// interactive clients that switch between multiple daemon endpoints in one
// process and therefore cannot rely on the package-global auth resolution
// path.
type TargetAuth struct {
	Token               string
	AllowInsecure       bool
	TrustPrivateNetwork bool
}

// NewHTTPClient returns an *http.Client for a URL-only compatibility caller.
// DaemonName and WorkspaceStart temporarily recover catalog credentials for
// callers that still discard source provenance. Callers that resolved their
// daemon through this package should use NewHTTPClientForResolved, which
// consumes the selected token and transport policy without re-reading config.
func NewHTTPClient(ctx context.Context, baseURL string, opts Opts) (*http.Client, error) {
	if opts.DaemonName != "" {
		target, err := namedDaemonTargetForBaseURL(opts.DaemonName, baseURL)
		if err != nil {
			return nil, err
		}
		if strings.TrimRight(baseURL, "/") != target.BaseURL {
			return nil, fmt.Errorf("daemon %q resolved to %s, not %s",
				target.Name, target.BaseURL, strings.TrimRight(baseURL, "/"))
		}
		if !target.Local {
			auth := resolveAuthConfig()
			if token := authTokenEnvOverride(); token != "" {
				return NewHTTPClientForTarget(ctx, baseURL,
					TargetAuth{
						Token:               token,
						AllowInsecure:       target.AllowInsecure,
						TrustPrivateNetwork: auth.TrustPrivateNetwork,
					}, opts)
			}
			return NewHTTPClientForTarget(ctx, baseURL,
				TargetAuth{
					Token:               target.Token,
					AllowInsecure:       target.AllowInsecure,
					TrustPrivateNetwork: auth.TrustPrivateNetwork,
				}, opts)
		}
		if target.Token != "" {
			return NewHTTPClientWithBearer(ctx, baseURL, target.Token, opts)
		}
		auth := resolveAuthConfig()
		return newHTTPClientWithAuth(ctx, baseURL, auth, opts)
	}
	if auth, ok, err := activeRemoteTargetAuthForBaseURL(baseURL, opts.WorkspaceStart); err != nil {
		return nil, err
	} else if ok {
		return NewHTTPClientForTarget(ctx, baseURL, auth, opts)
	}
	auth := resolveAuthConfig()
	return newHTTPClientWithAuth(ctx, baseURL, auth, opts)
}

// NewHTTPClientWithBearer returns an HTTP client bound to baseURL with an
// explicit bearer token. Empty token preserves the plain no-auth transport,
// but the target is still validated before the client is returned.
// TrustPrivateNetwork is still resolved from config so private-network
// federation callers honour the operator opt-in even when supplying their
// own token.
func NewHTTPClientWithBearer(ctx context.Context, baseURL, token string, opts Opts) (*http.Client, error) {
	auth := resolveAuthConfig()
	auth.Token = token
	return newHTTPClientWithAuth(ctx, baseURL, auth, opts)
}

// NewHTTPClientForTarget returns an HTTP client for a fully-resolved daemon
// target. Unlike NewHTTPClient and NewHTTPClientWithBearer, it does not read
// global auth config; the supplied TargetAuth is the complete bearer policy
// for this client.
func NewHTTPClientForTarget(ctx context.Context, baseURL string, auth TargetAuth, opts Opts) (*http.Client, error) {
	c, err := newHTTPClientWithoutAuth(ctx, baseURL, opts)
	if err != nil {
		return nil, err
	}
	rt, err := authBearerTransport(c.Transport, auth.Token, baseURL,
		auth.TrustPrivateNetwork, auth.AllowInsecure)
	if err != nil {
		return nil, err
	}
	c.Transport = rt
	return c, nil
}

// NewHTTPClientForResolved constructs a client from an already-resolved
// daemon. The endpoint, token, and target policy are consumed as supplied;
// construction does not re-read remote configuration, catalog credentials,
// or global authentication policy.
func NewHTTPClientForResolved(ctx context.Context, d ResolvedDaemon, opts Opts) (*http.Client, error) {
	if d.BaseURL == "" {
		return nil, errors.New("resolved daemon has no base URL")
	}
	if d.UnixSocket != "" {
		c := unixClient(d.UnixSocket, opts)
		rt, err := authBearerTransport(c.Transport, d.Token, d.BaseURL,
			d.TrustPrivateNetwork, d.AllowInsecure)
		if err != nil {
			return nil, err
		}
		c.Transport = rt
		return c, nil
	}
	return NewHTTPClientForTarget(ctx, d.BaseURL, TargetAuth{
		Token:               d.Token,
		AllowInsecure:       d.AllowInsecure,
		TrustPrivateNetwork: d.TrustPrivateNetwork,
	}, opts)
}

func newHTTPClientWithAuth(ctx context.Context, baseURL string, auth config.AuthConfig, opts Opts) (*http.Client, error) {
	c, err := newHTTPClientWithoutAuth(ctx, baseURL, opts)
	if err != nil {
		return nil, err
	}
	allowInsecure := opts.AllowInsecure || remoteAllowInsecureForBaseURL(baseURL, opts.WorkspaceStart)
	rt, err := authBearerTransport(c.Transport, auth.Token, baseURL,
		auth.TrustPrivateNetwork, allowInsecure)
	if err != nil {
		return nil, err
	}
	c.Transport = rt
	return c, nil
}

func newHTTPClientWithoutAuth(ctx context.Context, baseURL string, opts Opts) (*http.Client, error) {
	if !strings.HasPrefix(baseURL, UnixBase) {
		return tcpClient(opts)
	}
	ns, err := daemon.NewNamespace()
	if err != nil {
		return nil, err
	}
	for candidate, err := range liveDaemons(ctx, ns.DataDir) {
		if err != nil {
			if errors.Is(err, ErrLocalDaemonUnreachable) {
				continue
			}
			return nil, err
		}
		if candidate.UnixSocket == "" {
			continue
		}
		return unixClient(candidate.UnixSocket, opts), nil
	}
	return nil, errors.New("no unix-socket daemon found")
}

func tcpClient(opts Opts) (*http.Client, error) {
	c := &http.Client{Timeout: opts.Timeout}
	if opts.ResponseHeaderTimeout == 0 {
		return c, nil
	}
	// Clone http.DefaultTransport instead of building a bare *http.Transport
	// so we keep ProxyFromEnvironment, dial timeouts, TLS handshake timeout,
	// and HTTP/2 negotiation. Streaming clients have no overall Client.Timeout,
	// so a missing default could let DNS/TCP/TLS phases hang indefinitely
	// before ResponseHeaderTimeout could fire.
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not *http.Transport")
	}
	clone := t.Clone()
	clone.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	c.Transport = clone
	return c, nil
}

// unixClient builds a client whose transport dials the named socket.
func unixClient(socket string, opts Opts) *http.Client {
	t := UnixTransport(socket)
	if opts.ResponseHeaderTimeout > 0 {
		t.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	}
	return &http.Client{Transport: t, Timeout: opts.Timeout}
}
