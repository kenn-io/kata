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
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/pathresolve"
)

const docbankService = "docbank"

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
	ep, key, err := l.locate(ctx)
	if err != nil {
		return err
	}
	conn, err := proveKey(ctx, ep.Address, key)
	if err != nil {
		return fmt.Errorf("docbank at %s: %w", ep.Address, err)
	}
	return conn.Close()
}

// locate finds the running Docbank, starting it when none is running, and
// returns its endpoint and the API key its runtime record names.
func (l Local) locate(ctx context.Context) (kitdaemon.Endpoint, string, error) {
	home, err := LocalHome()
	if err != nil {
		return kitdaemon.Endpoint{}, "", err
	}
	rec, found, err := discoverDocbank(ctx, home)
	if err != nil {
		return kitdaemon.Endpoint{}, "", err
	}
	if !found {
		start := l.Start
		if start == nil {
			start = startDocbank
		}
		if err := start(ctx, home); err != nil {
			return kitdaemon.Endpoint{}, "", fmt.Errorf("start docbank in %s: %w", home, err)
		}
		if rec, found, err = discoverDocbank(ctx, home); err != nil {
			return kitdaemon.Endpoint{}, "", err
		}
		if !found {
			return kitdaemon.Endpoint{}, "", fmt.Errorf("docbank is not running in %s after starting it", home)
		}
	}
	key := rec.Metadata["api_key"]
	if key == "" {
		return kitdaemon.Endpoint{}, "", fmt.Errorf("the Docbank in %s did not publish an API key", home)
	}
	ep := rec.Endpoint()
	if ep.Network != "tcp" {
		return kitdaemon.Endpoint{}, "", fmt.Errorf("the Docbank in %s listens on an unsupported network %q", home, ep.Network)
	}
	return ep, key, nil
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
		return nil, fmt.Errorf("set connection deadline: %w", err)
	}
	// Reads on the raw connection ignore ctx, so its end cuts the proof short.
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("prepare identity check: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/daemon/key-challenge?nonce="+hex.EncodeToString(nonce), nil)
	if err != nil {
		return nil, fmt.Errorf("prepare identity check: %w", err)
	}
	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("identity check failed: %w", err)
	}
	br := bufio.NewReader(io.LimitReader(conn, 2*keyProofLimit))
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("identity check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity check answered HTTP %d; this is not the Docbank that published the key", resp.StatusCode)
	}
	if resp.Close {
		return nil, errors.New("the Docbank closed the connection before its identity could be verified")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, keyProofLimit+1))
	if err != nil {
		return nil, fmt.Errorf("identity check failed: %w", err)
	}
	if len(raw) > keyProofLimit || br.Buffered() != 0 {
		return nil, errors.New("the Docbank sent an unexpected response; its identity could not be verified")
	}
	var body struct {
		Proof string `json:"proof"`
	}
	if err = json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("the Docbank sent an invalid identity check response: %w", err)
	}
	got, err := hex.DecodeString(body.Proof)
	if err != nil {
		return nil, fmt.Errorf("the Docbank sent an invalid identity check response: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(keyProofDomain))
	mac.Write(nonce)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, errors.New("the listener did not prove it holds Docbank's API key; refusing to send the key")
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear connection deadline: %w", err)
	}
	return conn, nil
}

// discoverDocbank reports a missing home or an unreachable record as not
// running; List would create the missing home, so it is never asked.
func discoverDocbank(ctx context.Context, home string) (kitdaemon.RuntimeRecord, bool, error) {
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return kitdaemon.RuntimeRecord{}, false, nil
	}
	// The runtime store rejects a symlinked directory; Docbank resolves its own root the same way.
	// pathresolve also follows Windows directory junctions, which filepath.EvalSymlinks does not.
	resolved, err := pathresolve.EvalSymlinks(home)
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
		ep, key, err := l.locate(ctx)
		if err != nil {
			slog.WarnContext(ctx, "local docbank unavailable", "error", err)
			return "", "", nil, &Error{}
		}
		// The key travels only over connections whose listener proved it holds the key: the record's
		// PID can outlive the listener, so another user could bind the freed port and answer the ping.
		// net/http detaches dials from request cancellation; this transport serves only this request,
		// so the proof uses the request's context.
		proven := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
			conn, err := proveKey(ctx, ep.Address, key)
			if err != nil {
				slog.WarnContext(ctx, "local docbank unavailable", "address", ep.Address, "error", err)
			}
			return conn, err
		}}
		return ep.BaseURL(), key, noRedirectClient(proven), nil
	}}
}
