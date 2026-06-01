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

// PingInfo is the live daemon identity returned by /api/v1/ping.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid,omitempty"`
}

// Discover scans the namespace's runtime files and returns the base URL of
// the first daemon that passes /api/v1/ping. The bool is false when none
// respond — auto-start logic lives separately in EnsureRunning so callers
// that should never spawn (e.g. health probes) can opt out.
func Discover(ctx context.Context, dataDir string) (string, bool) {
	recs, err := (kitdaemon.RuntimeStore{Dir: dataDir}).List()
	if err != nil {
		return "", false
	}
	for _, r := range recs {
		if !kitdaemon.ProcessAlive(r.PID) {
			continue
		}
		if url, ok := pingAddress(ctx, r.Endpoint().ConfigAddress()); ok {
			return url, true
		}
	}
	return "", false
}

// pingAddress probes /api/v1/ping at a runtime-file address. Returns the
// base URL the caller should use to reach the daemon. Version/service
// compatibility is enforced by EnsureRunning; plain Discover only needs
// the endpoint to answer the kata liveness shape.
func pingAddress(ctx context.Context, address string) (string, bool) {
	url, _, ok := probeAddress(ctx, address)
	return url, ok
}

func probeAddress(ctx context.Context, address string) (string, PingInfo, bool) {
	if strings.HasPrefix(address, "unix://") {
		path := strings.TrimPrefix(address, "unix://")
		client := &http.Client{Transport: UnixTransport(path), Timeout: 1 * time.Second}
		info, err := Probe(ctx, client, UnixBase)
		if err == nil {
			return UnixBase, info, true
		}
		return "", PingInfo{}, false
	}
	url := "http://" + address
	client := &http.Client{Timeout: 1 * time.Second}
	info, err := Probe(ctx, client, url)
	if err == nil {
		return url, info, true
	}
	return "", PingInfo{}, false
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
}

// TargetAuth is explicit per-target bearer configuration. It is used by
// interactive clients that switch between multiple daemon endpoints in one
// process and therefore cannot rely on the package-global auth resolution
// path.
type TargetAuth struct {
	Token         string
	AllowInsecure bool
}

// NewHTTPClient returns an *http.Client whose transport matches baseURL —
// unix-socket dialing when baseURL == UnixBase, plain TCP otherwise. Pair
// with the URL returned by Discover/EnsureRunning. We re-scan and re-probe
// runtime files for unix endpoints so a stale record listed before a live
// one cannot redirect us to a dead socket.
//
// When KATA_AUTH_TOKEN or [auth].token in <KATA_HOME>/config.toml is set,
// the returned client transparently attaches Authorization: Bearer <token>
// to every outgoing request via a wrapping RoundTripper. This matches the
// daemon's bearer-auth middleware so token-protected daemons stay usable
// from the first-party CLI/TUI without callers having to plumb the header
// through every request site.
func NewHTTPClient(ctx context.Context, baseURL string, opts Opts) (*http.Client, error) {
	auth := resolveAuthConfig()
	return newHTTPClientWithAuth(ctx, baseURL, auth, opts)
}

// NewHTTPClientWithBearer returns an HTTP client bound to baseURL with an
// explicit bearer token. Empty token preserves the plain no-auth client path.
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
	rt, err := explicitBearerTransport(c.Transport, auth.Token, baseURL, auth.AllowInsecure)
	if err != nil {
		return nil, err
	}
	c.Transport = rt
	return c, nil
}

func newHTTPClientWithAuth(ctx context.Context, baseURL string, auth config.AuthConfig, opts Opts) (*http.Client, error) {
	c, err := newHTTPClientWithoutAuth(ctx, baseURL, opts)
	if err != nil {
		return nil, err
	}
	allowInsecure := opts.AllowInsecure || remoteAllowInsecureForBaseURL(baseURL, "")
	rt, err := authBearerTransport(c.Transport, auth.Token, baseURL, auth.TrustPrivateNetwork, allowInsecure)
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
	return unixClientFromRuntime(ctx, opts)
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

func unixClientFromRuntime(ctx context.Context, opts Opts) (*http.Client, error) {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return nil, err
	}
	recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if !kitdaemon.ProcessAlive(r.PID) {
			continue
		}
		ep := r.Endpoint()
		if !ep.IsUnix() {
			continue
		}
		path := ep.Address
		probe := &http.Client{Transport: UnixTransport(path), Timeout: 1 * time.Second}
		if !Ping(ctx, probe, UnixBase) {
			continue
		}
		t := UnixTransport(path)
		if opts.ResponseHeaderTimeout > 0 {
			t.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
		}
		return &http.Client{Transport: t, Timeout: opts.Timeout}, nil
	}
	return nil, errors.New("no unix-socket daemon found")
}
