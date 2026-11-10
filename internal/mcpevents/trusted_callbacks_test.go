package mcpevents

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestTrustedCallbackPolicy(t *testing.T) {
	for _, pin := range []string{"10.1.2.3", "172.16.1.2", "192.168.1.2", "100.64.0.1", "fd00::1"} {
		t.Run(pin, func(t *testing.T) {
			entries := []TrustedCallback{{Origin: "https://EXAMPLE.com:443/", Addresses: []string{pin}}}
			policy, err := CompileTrustedCallbacks(entries)
			if err != nil {
				t.Fatal(err)
			}
			entries[0].Addresses[0] = "127.0.0.1"
			w, server := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				checkFixtureSignature(t, r, body, fixtureSecret(1, 32))
				if strings.Contains(string(body), `"verification"`) {
					_, _ = rw.Write(body)
				} else {
					rw.WriteHeader(202)
				}
			})
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			resolves := 0
			w = NewWebhookClient(WebhookOptions{TrustedCallbacks: policy, RootCAs: roots, Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
				resolves++
				return []netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil
			}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != net.JoinHostPort(pin, "443") {
					t.Errorf("dial=%s", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}})
			if err := w.Verify(t.Context(), fixtureTLSCallback, "sub", fixtureSecret(1, 32)); err != nil {
				t.Fatal(err)
			}
			receipt, err := w.Send(t.Context(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(1, 32)})
			if err != nil || receipt.StatusCode != 202 {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if resolves != 0 {
				t.Fatal("trusted callback used DNS")
			}
			for _, raw := range []string{"https://other.example/events", "https://example.com:444/events", "https://sub.example.com/events"} {
				if err := w.ValidateCallback(t.Context(), raw); safeWebhookError(err).Reason != "destination_not_allowed" {
					t.Fatalf("nonexact origin accepted: %s %v", raw, err)
				}
			}
		})
	}
}

func TestTrustedCallbackPolicyRejectsInvalidConfiguration(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://user@example.com", "https://*.example.com", "https://localhost", "https://service.local", "https://example.com/path", "https://example.com/?", "https://example.com/#", "https://example.com:0", "https://example.com:", "https://example.com/%2f"} {
		if _, err := CompileTrustedCallbacks([]TrustedCallback{{Origin: origin, Addresses: []string{"10.0.0.1"}}}); err == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
	for _, pin := range []string{"127.0.0.1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "8.8.8.8", "192.0.2.1", "::1", "fe80::1", "::", "ff00::1", "2001:db8::1", "fd00::1%eth0", "10.0.0.0/8", "receiver.example"} {
		if _, err := CompileTrustedCallbacks([]TrustedCallback{{Origin: "https://example.com", Addresses: []string{pin}}}); err == nil {
			t.Errorf("accepted pin %q", pin)
		}
	}
	for _, entries := range [][]TrustedCallback{
		{{Origin: "https://example.com"}},
		{{Origin: "https://example.com", Addresses: []string{"10.0.0.1", "::ffff:10.0.0.1"}}},
		{{Origin: "https://example.com", Addresses: []string{"10.0.0.1"}}, {Origin: "https://EXAMPLE.com:443/", Addresses: []string{"10.0.0.2"}}},
		{{Origin: "https://10.0.0.1", Addresses: []string{"10.0.0.2"}}},
	} {
		if _, err := CompileTrustedCallbacks(entries); err == nil {
			t.Errorf("accepted invalid entries: %+v", entries)
		}
	}
}

func TestTrustedCallbackTLSAndLiteral(t *testing.T) {
	_, server := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(204) })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, tc := range []struct {
		origin, pin, callback string
		roots                 *x509.CertPool
	}{
		{"https://example.com", "10.0.0.1", fixtureTLSCallback, nil},
		{"https://wrong.example", "10.0.0.1", "https://wrong.example/events", roots},
		{"https://10.0.0.1", "10.0.0.1", "https://10.0.0.1/events", roots},
	} {
		policy, err := CompileTrustedCallbacks([]TrustedCallback{{Origin: tc.origin, Addresses: []string{tc.pin}}})
		if err != nil {
			t.Fatal(err)
		}
		w := NewWebhookClient(WebhookOptions{TrustedCallbacks: policy, RootCAs: tc.roots, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}})
		_, err = w.Send(t.Context(), tc.callback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(1, 32)})
		if safeWebhookError(err).Reason != "tls_error" {
			t.Fatalf("missing TLS rejection: %v", err)
		}
	}
}
