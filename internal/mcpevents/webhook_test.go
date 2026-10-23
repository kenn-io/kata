package mcpevents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const fixtureCallback = "https://receiver.example/events?route=inbox"

func fixtureSecret(b byte, size int) string {
	return "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, size))
}

// The fixture replaces only the network socket: production URL validation,
// public-IP policy, TLS hostname checking and HTTP/signature serialization run.
func fixtureWebhook(t *testing.T, handler http.HandlerFunc) (*WebhookClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	w := NewWebhookClient(WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				t.Errorf("dial did not pin validated public IP: %q", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		RootCAs: roots,
	})
	// httptest's certificate contains example.com; callback TLS must preserve it.
	return w, server
}

const fixtureTLSCallback = "https://example.com/events?route=inbox"

func checkFixtureSignature(t *testing.T, r *http.Request, body []byte, keys ...string) {
	t.Helper()
	if r.Header.Get("Content-Type") != "application/json" {
		t.Error("missing JSON content type")
	}
	ts := r.Header.Get("Webhook-Timestamp")
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Errorf("invalid timestamp %q", ts)
	}
	parts := strings.Fields(r.Header.Get("Webhook-Signature"))
	if len(parts) != len(keys) {
		t.Fatalf("signatures=%d, want %d", len(parts), len(keys))
	}
	for i, key := range keys {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(key, "whsec_"))
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, decoded)
		_, _ = mac.Write([]byte(r.Header.Get("Webhook-Id") + "." + ts + "."))
		_, _ = mac.Write(body)
		got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[i], "v1,"))
		if err != nil || !strings.HasPrefix(parts[i], "v1,") || !hmac.Equal(got, mac.Sum(nil)) {
			t.Errorf("signature %d does not authenticate exact bytes", i)
		}
	}
}

func TestWebhookVerificationChallenge(t *testing.T) {
	key := fixtureSecret(7, 32)
	seen := make(map[string]bool)
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		checkFixtureSignature(t, r, body, key)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/events?route=inbox" || r.Host != "example.com" || r.TLS.ServerName != "example.com" {
			t.Error("callback request did not retain method, URL, Host and TLS SNI")
		}
		if r.Header.Get("X-MCP-Subscription-Id") != "sub_fixture" {
			t.Error("subscription header missing")
		}
		var challenge struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if json.Unmarshal(body, &challenge) != nil || challenge.Type != "verification" || len(challenge.Challenge) < 32 || seen[challenge.Challenge] {
			t.Error("invalid or reused challenge")
		}
		seen[challenge.Challenge] = true
		_ = json.NewEncoder(rw).Encode(map[string]string{"challenge": challenge.Challenge})
	})
	for range 2 {
		if err := w.Verify(context.Background(), fixtureTLSCallback, "sub_fixture", key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebhookExactBytesAndRotationSignatures(t *testing.T) {
	keys := []string{fixtureSecret(2, 24), fixtureSecret(3, 64)}
	body := []byte("{ \"eventId\": \"evt_stable\",\n\"data\":{\"unicode\":\"☃\"}}\n")
	var timestamps []string
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) || r.Header.Get("Webhook-Id") != "evt_stable" || r.Header.Get("X-MCP-Subscription-Id") != "sub_fixture" {
			t.Error("delivery mutated exact envelope or stable IDs")
		}
		checkFixtureSignature(t, r, got, keys...)
		timestamps = append(timestamps, r.Header.Get("Webhook-Timestamp"))
		rw.WriteHeader(http.StatusAccepted)
	})
	for _, second := range []int64{1790000000, 1790000015} {
		w.now = func() time.Time { return time.Unix(second, 0) }
		receipt, err := w.Send(context.Background(), fixtureTLSCallback, "evt_stable", "sub_fixture", body, keys)
		if err != nil || receipt.StatusCode != 202 {
			t.Fatalf("receipt=%+v error=%v", receipt, err)
		}
	}
	if timestamps[0] != "1790000000" || timestamps[1] != "1790000015" {
		t.Errorf("retry did not use fresh signing timestamp: %v", timestamps)
	}
}

func TestWebhookRejectsRebindingRedirectAndPrivateIPs(t *testing.T) {
	key := fixtureSecret(2, 32)
	var received atomic.Int32
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		rw.Header().Set("Location", "https://127.0.0.1/secret")
		rw.WriteHeader(http.StatusTemporaryRedirect)
	})
	receipt, err := w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{key})
	if err != nil || receipt.StatusCode != 307 || received.Load() != 1 {
		t.Fatalf("redirect followed or lost: %+v %v", receipt, err)
	}
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	_, err = w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{key})
	if err == nil || received.Load() != 1 {
		t.Fatal("DNS rebinding reached receiver")
	}
	for _, addresses := range [][]string{
		{"8.8.8.8", "10.0.0.1"}, {"::1"}, {"::ffff:127.0.0.1"}, {"100.64.0.1"}, {"169.254.1.1"}, {"192.0.2.1"}, {"198.51.100.1"}, {"203.0.113.1"}, {"198.18.0.1"}, {"240.1.1.1"}, {"ff02::1"}, {"2001:db8::1"}, {"64:ff9b::7f00:1"}, {"2002:7f00:1::"}, {"2001::ffff:80ff:fffe"}, {"fe80::1%eth0"}, {},
	} {
		t.Run(strings.Join(addresses, ","), func(t *testing.T) {
			w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
				var result []netip.Addr
				for _, raw := range addresses {
					result = append(result, netip.MustParseAddr(raw))
				}
				return result, nil
			}
			if _, err := w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{key}); err == nil {
				t.Fatal("prohibited DNS answer accepted")
			}
		})
	}
	if received.Load() != 1 {
		t.Fatal("prohibited DNS answer caused network activity")
	}
}

func TestWebhookNoDaemonHeadersOrProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("KATA_AUTH_TOKEN", "fixture-daemon-token")
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Kata-Web-Session", "X-Kata-CSRF", "Forwarded", "X-Forwarded-For"} {
			if r.Header.Get(name) != "" {
				t.Errorf("callback received authority header %s", name)
			}
		}
		rw.WriteHeader(204)
	})
	if _, err := w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(2, 32)}); err != nil {
		t.Fatal(err)
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("used environment proxy")
	}
}

func TestWebhookValidationBeforeNetwork(t *testing.T) {
	var calls int
	w := NewWebhookClient(WebhookOptions{Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
		calls++
		return nil, errors.New("secret callback details")
	}})
	key := fixtureSecret(1, 32)
	for _, callback := range []string{"http://receiver.example/events", "https://user:pass@receiver.example/events", fixtureCallback + "#", fixtureCallback + "#fragment", "https://127.0.0.1/", "https://[::1]/", "https://localhost/", "https://2130706433/", "https://receiver.example:0/", "https://receiver.example:65536/"} {
		if _, err := w.Send(context.Background(), callback, "evt", "sub", []byte(`{}`), []string{key}); err == nil {
			t.Errorf("accepted callback %q", callback)
		}
	}
	for _, secret := range []string{"", "secret", fixtureSecret(1, 23), fixtureSecret(1, 65), key + "\n", "whsec_!!!!"} {
		if err := w.Verify(context.Background(), fixtureCallback, "sub", secret); err == nil {
			t.Error("accepted invalid signing secret")
		}
	}
	for _, keys := range [][]string{nil, {key, key, key}} {
		if _, err := w.Send(context.Background(), fixtureCallback, "evt", "sub", []byte(`{}`), keys); err == nil {
			t.Error("accepted invalid key count")
		}
	}
	if _, err := w.Send(context.Background(), fixtureCallback, "evt", "sub", bytes.Repeat([]byte{'x'}, 256*1024+1), []string{key}); err == nil {
		t.Error("accepted oversized envelope")
	}
	if calls != 0 {
		t.Errorf("invalid callback/secret/envelope resolved DNS %d times", calls)
	}
}

func TestWebhookVerificationFailureIsBoundedAndSanitized(t *testing.T) {
	key := fixtureSecret(1, 32)
	for _, mode := range []string{"wrong", "non-json", "oversized", "status"} {
		t.Run(mode, func(t *testing.T) {
			w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
				switch mode {
				case "wrong":
					_, _ = io.WriteString(rw, `{"challenge":"secret-response-body"}`)
				case "non-json":
					_, _ = io.WriteString(rw, "secret-response-body")
				case "oversized":
					var input struct {
						Challenge string `json:"challenge"`
					}
					_ = json.NewDecoder(r.Body).Decode(&input)
					_ = json.NewEncoder(rw).Encode(map[string]string{"challenge": input.Challenge, "padding": strings.Repeat("x", 64*1024)})
				case "status":
					rw.WriteHeader(500)
				}
			})
			err := w.Verify(context.Background(), fixtureTLSCallback, "sub", key)
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), key) {
				t.Fatalf("unsafe or absent error: %v", err)
			}
			var safe *WebhookError
			if !errors.As(err, &safe) || safe.Reason == "" {
				t.Fatalf("uncategorized error: %v", err)
			}
		})
	}
	w := NewWebhookClient(WebhookOptions{Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("secret DNS callback body")
	}})
	if err := w.Verify(context.Background(), fixtureCallback, "sub", key); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("resolver error leaked: %v", err)
	}
}

func TestWebhookTimeoutAndReceipt(t *testing.T) {
	key := fixtureSecret(1, 32)
	synctest.Test(t, func(t *testing.T) {
		w := NewWebhookClient()
		w.client.Transport = eventRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := w.Send(ctx, fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{key})
		var safe *WebhookError
		if !errors.As(err, &safe) || safe.Reason != "timeout" {
			t.Fatalf("timeout not categorized: %v", err)
		}
	})
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"120", 2 * time.Minute}, {"999999999999999999999999999", 5 * time.Minute}, {"600", 5 * time.Minute}, {"-2", 0}, {"invalid", 0}, {"Thu, 01 Jan 1970 00:02:00 GMT", 2 * time.Minute},
	} {
		t.Run(tc.header, func(t *testing.T) {
			w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Retry-After", tc.header)
				rw.WriteHeader(429)
			})
			w.now = func() time.Time { return time.Unix(0, 0) }
			receipt, err := w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{key})
			if err != nil || receipt.StatusCode != 429 || receipt.RetryAfter != tc.want {
				t.Fatalf("receipt=%+v error=%v want retry=%v", receipt, err, tc.want)
			}
		})
	}
}

func TestWebhookDialHasIndependentDeadline(t *testing.T) {
	// net/http can detach a connection attempt from the request's deadline.
	// Even then DNS resolution and the socket dial must remain bounded.
	w := NewWebhookClient(WebhookOptions{Resolve: func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("callback dial lacks its own bounded deadline")
		}
		return nil, errors.New("fixture resolver refusal")
	}})
	_, err := w.dialPublic(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("resolver refusal was ignored")
	}
}

func TestWebhookTLSHostAndRoots(t *testing.T) {
	var received atomic.Int32
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) { received.Add(1); rw.WriteHeader(204) })
	key := fixtureSecret(1, 32)
	for _, callback := range []string{fixtureCallback, fixtureTLSCallback} {
		if callback == fixtureTLSCallback {
			// The callback has the correct name but the root is no longer trusted.
			w = NewWebhookClient(WebhookOptions{Resolve: w.resolve, DialContext: w.dial})
		}
		_, err := w.Send(context.Background(), callback, "evt", "sub", []byte(`{}`), []string{key})
		var safe *WebhookError
		if !errors.As(err, &safe) || safe.Reason != "tls_error" {
			t.Fatalf("invalid certificate was not rejected safely: %v", err)
		}
	}
	if received.Load() != 0 {
		t.Fatal("unverified TLS request reached handler")
	}
}

func TestWebhookPublicIPv6PinsNumericDial(t *testing.T) {
	var received atomic.Int32
	w, server := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) { received.Add(1); rw.WriteHeader(204) })
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2001:4860:4860::8888")}, nil
	}
	w.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "[2001:4860:4860::8888]:443" {
			t.Errorf("IPv6 not pinned: %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	if _, err := w.Send(context.Background(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(1, 32)}); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatal("public IPv6 destination not delivered")
	}
}
