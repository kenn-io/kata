package docbank

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/config"
)

const docbankService = "docbank"

// New returns the client for cfg, or nil when Docbank is not configured.
// cfg must already be normalized.
func New(cfg config.DocbankConfig) *Client {
	switch {
	case !cfg.Enabled():
		return nil
	case cfg.UsesLocal():
		return NewLocalClient(Local{})
	default:
		return NewClient(cfg.URL, cfg.ResolveKey)
	}
}

// LocalHome returns Docbank's home: DOCBANK_HOME when set, else ~/.docbank.
func LocalHome() (string, error) {
	if h := os.Getenv("DOCBANK_HOME"); h != "" {
		abs, err := filepath.Abs(h)
		if err != nil {
			return "", fmt.Errorf("resolve DOCBANK_HOME: %w", err)
		}
		return abs, nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory for the Docbank home: %w", err)
	}
	return filepath.Join(user, ".docbank"), nil
}

// Local finds the Docbank running on this machine through its runtime record.
// The home is always LocalHome.
type Local struct {
	// Start launches Docbank in home; nil runs `docbank daemon start`.
	Start func(ctx context.Context, home string) error
}

// Ready checks that a Docbank on this machine is running, starting it when it
// is not, and that it holds the key its runtime record names.
func (l Local) Ready(ctx context.Context) error {
	_, _, conn, err := l.connect(ctx)
	if err != nil {
		return err
	}
	return conn.Close()
}

// connect returns the running Docbank's base URL, its API key, and a
// connection to it that has proven it holds that key. The key must only travel
// over that connection: the record's PID can outlive the listener, so another
// user could otherwise bind the freed port and answer the public ping.
func (l Local) connect(ctx context.Context) (base, key string, conn net.Conn, err error) {
	home, err := LocalHome()
	if err != nil {
		return "", "", nil, err
	}
	rec, found, err := discoverDocbank(ctx, home)
	if err != nil {
		return "", "", nil, err
	}
	if !found {
		start := l.Start
		if start == nil {
			start = startDocbank
		}
		if err := start(ctx, home); err != nil {
			return "", "", nil, fmt.Errorf("start docbank in %s: %w", home, err)
		}
		if rec, found, err = discoverDocbank(ctx, home); err != nil {
			return "", "", nil, err
		}
		if !found {
			return "", "", nil, fmt.Errorf("docbank is not running in %s after starting it", home)
		}
	}
	key = rec.Metadata["api_key"]
	if key == "" {
		return "", "", nil, fmt.Errorf("docbank runtime record in %s has no api_key", home)
	}
	ep := rec.Endpoint()
	if ep.Network != "tcp" {
		return "", "", nil, fmt.Errorf("docbank runtime record in %s uses unsupported network %q", home, ep.Network)
	}
	conn, err = proveKey(ctx, ep.Address, key)
	if err != nil {
		return "", "", nil, fmt.Errorf("docbank at %s: %w", ep.Address, err)
	}
	return ep.BaseURL(), key, conn, nil
}

const (
	keyProofDomain  = "docbank-api-key-proof-v1\x00"
	keyProofTimeout = 5 * time.Second
	keyProofLimit   = 4 << 10
)

// proveKey dials address and returns the connection once its server has shown
// it knows key, without ever sending the key.
func proveKey(ctx context.Context, address, key string) (conn net.Conn, err error) {
	deadline := time.Now().Add(keyProofTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	c := conn
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set key proof deadline: %w", err)
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("make key proof nonce: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/daemon/key-challenge?nonce="+hex.EncodeToString(nonce), nil)
	if err != nil {
		return nil, fmt.Errorf("build key proof request: %w", err)
	}
	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("send key proof request: %w", err)
	}
	br := bufio.NewReader(io.LimitReader(conn, 2*keyProofLimit))
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("read key proof response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key proof request answered HTTP %d; this is not a Docbank that holds the recorded key", resp.StatusCode)
	}
	if resp.Close {
		return nil, errors.New("key proof response closes the connection")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, keyProofLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read key proof body: %w", err)
	}
	if len(raw) > keyProofLimit || br.Buffered() != 0 {
		return nil, errors.New("key proof response is larger than expected")
	}
	var body struct {
		Proof string `json:"proof"`
	}
	if err = json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode key proof: %w", err)
	}
	got, err := hex.DecodeString(body.Proof)
	if err != nil {
		return nil, fmt.Errorf("decode key proof: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(keyProofDomain))
	mac.Write(nonce)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, errors.New("listener did not prove it holds the recorded API key; refusing to send the key")
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear key proof deadline: %w", err)
	}
	return conn, nil
}

// pinnedTransport carries one request over a connection already proven to
// belong to Docbank and refuses to dial anything else.
type pinnedTransport struct {
	inner *http.Transport
	mu    sync.Mutex
	conn  net.Conn
}

func newPinnedClient(conn net.Conn) *http.Client {
	p := &pinnedTransport{conn: conn}
	p.inner = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			c := p.conn
			if c == nil {
				return nil, errors.New("docbank connection already used")
			}
			p.conn = nil
			return c, nil
		},
	}
	return noRedirectClient(p)
}

func (p *pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.inner.RoundTrip(req)
}

// CloseIdleConnections closes the proven connection when no request used it.
func (p *pinnedTransport) CloseIdleConnections() {
	p.mu.Lock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.mu.Unlock()
	p.inner.CloseIdleConnections()
}

// discoverDocbank reports a missing home or an unreachable record as not
// running; List would create the missing home, so it is never asked.
func discoverDocbank(ctx context.Context, home string) (kitdaemon.RuntimeRecord, bool, error) {
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return kitdaemon.RuntimeRecord{}, false, nil
	}
	// The runtime store rejects a symlinked directory; Docbank resolves its own root the same way.
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false, fmt.Errorf("resolve docbank home %s: %w", home, err)
	}
	home = resolved
	rec, _, found, err := kitdaemon.Discover(ctx, kitdaemon.RuntimeStore{Dir: home}, kitdaemon.DiscoverOptions{
		RequirePIDAlive: true,
		Probe:           kitdaemon.ProbeOptions{ExpectedService: docbankService},
	})
	if errors.Is(err, kitdaemon.ErrDaemonUnreachable) {
		return kitdaemon.RuntimeRecord{}, false, nil
	}
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false, fmt.Errorf("look for docbank in %s: %w", home, err)
	}
	return rec, found, nil
}

func startDocbank(ctx context.Context, home string) error {
	bin, err := exec.LookPath("docbank")
	if err != nil {
		return errors.New("docbank is not running and the docbank command is not on PATH; install Docbank or start it yourself")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon", "start") //nolint:gosec // bin is the docbank binary found on PATH; the arguments are constant
	cmd.Env = append(os.Environ(), "DOCBANK_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docbank daemon start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NewLocalClient follows the local Docbank on every request, so a restart with
// a new port and key is picked up by the next call.
func NewLocalClient(l Local) *Client {
	slot := make(chan struct{}, 1)
	return &Client{target: func(ctx context.Context) (string, string, *http.Client, error) {
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		case <-ctx.Done():
			return "", "", nil, &Error{}
		}
		base, key, conn, err := l.connect(ctx)
		if err != nil {
			slog.WarnContext(ctx, "local docbank unavailable", "error", err)
			return "", "", nil, &Error{}
		}
		return base, key, newPinnedClient(conn), nil
	}}
}
