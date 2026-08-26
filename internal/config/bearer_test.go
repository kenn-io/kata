package config

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRoundTripper struct {
	calls         int
	authorization string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	r.authorization = req.Header.Get("Authorization")
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func TestBearerTransportCanonicalOrigin(t *testing.T) {
	tests := []struct {
		name              string
		target            string
		wantBaseCalls     int
		wantAuthorization string
		wantErr           bool
	}{
		{
			name:              "equivalent canonical origin",
			target:            "https://daemon.example/resource",
			wantBaseCalls:     1,
			wantAuthorization: "Bearer test-token",
		},
		{
			name:    "different port",
			target:  "https://daemon.example:8443/resource",
			wantErr: true,
		},
		{
			name:    "different scheme",
			target:  "http://daemon.example/resource",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &recordingRoundTripper{}
			transport := (BearerPolicy{}).Transport(base, "test-token", "https://Daemon.Example:443")
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			require.NoError(t, err)

			resp, err := transport.RoundTrip(req)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.NoError(t, resp.Body.Close())
			}
			assert.Equal(t, tt.wantBaseCalls, base.calls)
			assert.Equal(t, tt.wantAuthorization, base.authorization)
		})
	}
}

func TestBearerTransportCanonicalIPv6ZoneOrigin(t *testing.T) {
	base := &recordingRoundTripper{}
	transport := (BearerPolicy{}).Transport(
		base,
		"test-token",
		"https://[FE80::ABCD%25EnABC]:00443/configured/path",
	)
	req, err := http.NewRequest(
		http.MethodGet,
		"https://[fe80::abcd%25EnABC]/api/v1/ping",
		nil,
	)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, base.calls)
	assert.Equal(t, "Bearer test-token", base.authorization)
}

// TestBearerTargetPolicyMatrix is the accept/reject contract for bearer
// targets: every policy combination crossed with every target shape kata can
// be pointed at. It is the gate for folding the telescoping bearer helpers
// onto BearerPolicy — the refactor is only safe if this table's answers do
// not move.
func TestBearerTargetPolicyMatrix(t *testing.T) {
	targets := []struct {
		name string
		url  string
	}{
		{"https public host", "https://daemon.example/api/v1/ping"},
		{"http loopback literal", "http://127.0.0.1:7777/api/v1/ping"},
		{"http localhost", "http://localhost:7777/api/v1/ping"},
		{"http rfc1918 literal", "http://192.168.10.5:7777/api/v1/ping"},
		{"http cgnat literal", "http://100.64.0.5:7777/api/v1/ping"},
		{"http public host", "http://daemon.example/api/v1/ping"},
		{"http private hostname", "http://daemon-host:7777/api/v1/ping"},
		{"unix sentinel", "http://kata.invalid/api/v1/ping"},
	}
	// want[policyName][targetName] = accepted
	want := map[string]map[string]bool{
		"strict": {
			"https public host":     true,
			"http loopback literal": true,
			"http localhost":        true,
			"http rfc1918 literal":  false,
			"http cgnat literal":    false,
			"http public host":      false,
			"http private hostname": false,
			"unix sentinel":         true,
		},
		"trust private network": {
			"https public host":     true,
			"http loopback literal": true,
			"http localhost":        true,
			"http rfc1918 literal":  true,
			"http cgnat literal":    true,
			"http public host":      false,
			"http private hostname": false,
			"unix sentinel":         true,
		},
		"allow insecure plaintext": {
			"https public host":     true,
			"http loopback literal": true,
			"http localhost":        true,
			"http rfc1918 literal":  true,
			"http cgnat literal":    true,
			"http public host":      true,
			"http private hostname": true,
			"unix sentinel":         true,
		},
		"allow insecure plaintext and trust": {
			"https public host":     true,
			"http loopback literal": true,
			"http localhost":        true,
			"http rfc1918 literal":  true,
			"http cgnat literal":    true,
			"http public host":      true,
			"http private hostname": true,
			"unix sentinel":         true,
		},
	}
	policies := []struct {
		name   string
		policy BearerPolicy
	}{
		{"strict", BearerPolicy{}},
		{"trust private network", BearerPolicy{TrustPrivateNetwork: true}},
		{"allow insecure plaintext", BearerPolicy{AllowInsecurePlaintext: true}},
		{"allow insecure plaintext and trust", BearerPolicy{AllowInsecurePlaintext: true, TrustPrivateNetwork: true}},
	}

	for _, p := range policies {
		for _, target := range targets {
			t.Run(p.name+"/"+target.name, func(t *testing.T) {
				u, err := url.Parse(target.url)
				require.NoError(t, err)

				err = checkTargetUnderPolicy(p.policy, u)

				if want[p.name][target.name] {
					require.NoError(t, err, "target must be accepted under this policy")
					return
				}
				require.Error(t, err, "target must be rejected under this policy")
			})
		}
	}
}

// TestBearerTargetAllowInsecureShapeValidation pins that opting into plaintext
// does not opt out of the URL-shape checks needed to bind a bearer origin.
func TestBearerTargetAllowInsecureShapeValidation(t *testing.T) {
	tests := []struct {
		name   string
		policy BearerPolicy
		target string
	}{
		{
			name:   "allow insecure rejects unsupported scheme",
			policy: BearerPolicy{AllowInsecurePlaintext: true},
			target: "ftp://daemon.example/api/v1/ping",
		},
		{
			name:   "allow insecure rejects missing host",
			policy: BearerPolicy{AllowInsecurePlaintext: true},
			target: "http:///api/v1/ping",
		},
		{
			name:   "allow insecure and trust rejects unsupported scheme",
			policy: BearerPolicy{AllowInsecurePlaintext: true, TrustPrivateNetwork: true},
			target: "ftp://daemon.example/api/v1/ping",
		},
		{
			name:   "allow insecure and trust rejects missing host",
			policy: BearerPolicy{AllowInsecurePlaintext: true, TrustPrivateNetwork: true},
			target: "http:///api/v1/ping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.target)
			require.NoError(t, err)

			require.Error(t, checkTargetUnderPolicy(tt.policy, u), "invalid target shape must be rejected")
		})
	}
}

// checkTargetUnderPolicy is the seam the matrix runs through. It now calls the
// unified method; the table's answers must be unchanged from Task 4.
func checkTargetUnderPolicy(p BearerPolicy, u *url.URL) error {
	return p.CheckTargetURL(u)
}

// TestBearerTransportPerRequestPolicyMatrix pins that the per-request guard
// applies the same target rules as construction. The transport is always
// bound to a safe origin; only the request target varies, which is the
// redirect-exfiltration shape.
func TestBearerTransportPerRequestPolicyMatrix(t *testing.T) {
	tests := []struct {
		name        string
		policy      BearerPolicy
		target      string
		wantErr     bool
		wantReached bool
	}{
		{
			name:        "strict same-origin loopback accepted",
			policy:      BearerPolicy{},
			target:      "http://127.0.0.1:7777/api/v1/ping",
			wantReached: true,
		},
		{
			name:    "strict rejects plaintext private redirect",
			policy:  BearerPolicy{},
			target:  "http://192.168.10.5:7777/api/v1/ping",
			wantErr: true,
		},
		{
			name:    "trust private network still refuses cross-origin",
			policy:  BearerPolicy{TrustPrivateNetwork: true},
			target:  "http://192.168.10.5:7777/api/v1/ping",
			wantErr: true,
		},
		{
			name:    "allow insecure still refuses cross-origin",
			policy:  BearerPolicy{AllowInsecurePlaintext: true},
			target:  "http://daemon.example/api/v1/ping",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &recordingRoundTripper{}
			rt := tt.policy.Transport(base, "test-token", "http://127.0.0.1:7777")
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantReached {
				assert.Equal(t, 1, base.calls)
				assert.Equal(t, "Bearer test-token", base.authorization)
			} else {
				assert.Zero(t, base.calls, "a rejected target must not reach the base transport")
			}
		})
	}
}

// TestBearerPolicyConfigureClientEmptyToken pins decision D2: the target is
// validated whether or not a token is present. An empty token against a safe
// target configures nothing and succeeds; an unsafe target is an error even
// with no token, so a misconfigured endpoint is diagnosable instead of a
// silent no-op that fails later as an opaque 401.
func TestBearerPolicyConfigureClientEmptyToken(t *testing.T) {
	tests := []struct {
		name              string
		policy            BearerPolicy
		baseURL           string
		token             string
		wantErr           bool
		wantSameTransport bool
		wantAuthHeader    string
	}{
		{
			name:              "empty token safe target configures nothing",
			baseURL:           "http://127.0.0.1:7777",
			wantSameTransport: true,
		},
		{
			name:              "empty token unsafe target errors",
			baseURL:           "http://daemon.example",
			wantErr:           true,
			wantSameTransport: true,
		},
		{
			name:              "empty token private target errors without trust",
			baseURL:           "http://192.168.10.5:7777",
			wantErr:           true,
			wantSameTransport: true,
		},
		{
			name:              "empty token private target accepted under trust",
			policy:            BearerPolicy{TrustPrivateNetwork: true},
			baseURL:           "http://192.168.10.5:7777",
			wantSameTransport: true,
		},
		{
			name:              "empty token plaintext hostname accepted under allow insecure",
			policy:            BearerPolicy{AllowInsecurePlaintext: true},
			baseURL:           "http://daemon-host:7777",
			wantSameTransport: true,
		},
		{
			name:           "token safe target attaches header",
			baseURL:        "http://127.0.0.1:7777",
			token:          "secret",
			wantAuthHeader: "Bearer secret",
		},
		{
			name:              "token unsafe target errors",
			baseURL:           "http://daemon.example",
			token:             "secret",
			wantErr:           true,
			wantSameTransport: true,
		},
		{
			name:              "unix sentinel accepted without token",
			baseURL:           "http://kata.invalid",
			wantSameTransport: true,
		},
		{
			name:           "unix sentinel accepted with token",
			baseURL:        "http://kata.invalid",
			token:          "secret",
			wantAuthHeader: "Bearer secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &recordingRoundTripper{}
			c := &http.Client{Transport: base}

			err := tt.policy.ConfigureClient(c, tt.baseURL, tt.token)

			if tt.wantErr {
				require.Error(t, err)
				assert.Same(t, base, c.Transport,
					"a rejected target must leave the client's transport untouched")
				return
			}
			require.NoError(t, err)
			if tt.wantSameTransport {
				assert.Same(t, base, c.Transport,
					"an empty token must preserve the client's transport")
			}

			req, err := http.NewRequest(http.MethodGet, tt.baseURL+"/api/v1/ping", nil)
			require.NoError(t, err)
			resp, err := c.Transport.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tt.wantAuthHeader, base.authorization)
		})
	}
}

func TestBearerPolicyConfigureClientRejectsNilClient(t *testing.T) {
	err := BearerPolicy{}.ConfigureClient(nil, "http://127.0.0.1:7777", "secret")
	require.Error(t, err)
}
