package mcpevents

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"testing"

	"go.kenn.io/kata/internal/config"
)

func TestTrustedCallbackPolicy(t *testing.T) {
	const pin = "100.64.0.1"
	received := 0
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) {
		received++
		rw.WriteHeader(204)
	}, config.MCPEventsTrustedCallback{Origin: "https://EXAMPLE.com:443/", Addresses: []string{pin}})
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("trusted callback used DNS")
		return nil, nil
	}
	receipt, err := w.Send(t.Context(), fixtureTLSCallback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(1, 32)})
	if err != nil || receipt.StatusCode != 204 || received != 1 {
		t.Fatalf("trusted delivery failed: %+v %v count=%d", receipt, err, received)
	}
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil
	}
	for _, raw := range []string{"https://example.com:444/events", "https://sub.example.com/events"} {
		if err := w.validateCallback(t.Context(), fixtureURL(t, raw)); safeWebhookError(err).Reason != "destination_not_allowed" {
			t.Fatalf("nonexact origin accepted: %s %v", raw, err)
		}
	}
}

func TestTrustedCallbackPolicyConfiguration(t *testing.T) {
	for _, pin := range []string{"10.1.2.3", "172.16.1.2", "192.168.1.2", "100.64.0.1", "fd00::1"} {
		if _, err := CompileTrustedCallbacks([]config.MCPEventsTrustedCallback{{Origin: "https://example.com", Addresses: []string{pin}}}); err != nil {
			t.Errorf("valid pin %s: %v", pin, err)
		}
	}
	for _, origin := range []string{"https://events.corp.internal", "https://events.home.arpa", "https://receiver.local"} {
		policy, err := CompileTrustedCallbacks([]config.MCPEventsTrustedCallback{{Origin: origin, Addresses: []string{"10.0.0.1"}}})
		if err != nil {
			t.Fatalf("valid origin %s: %v", origin, err)
		}
		w := NewWebhookClient(WebhookOptions{TrustedCallbacks: policy, Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			t.Error("trusted private host used DNS")
			return nil, nil
		}})
		if err := w.validateStaticCallback(t.Context(), fixtureURL(t, origin+"/events")); err != nil {
			t.Error(err)
		}
		if err := w.validateCallback(t.Context(), fixtureURL(t, origin+"/events")); err != nil {
			t.Error(err)
		}
	}
	// Operator diagnostics identify only the configuration field and indexes,
	// never the supplied origin or address, even after earlier valid entries.
	checkError := func(entries []config.MCPEventsTrustedCallback, field string) {
		t.Helper()
		valid := config.MCPEventsTrustedCallback{Origin: "https://valid.example", Addresses: []string{"10.0.0.1"}}
		_, err := CompileTrustedCallbacks(append([]config.MCPEventsTrustedCallback{valid}, entries...))
		if err == nil || err.Error() != "invalid mcp_events.trusted_callbacks"+field {
			t.Errorf("expected sanitized field %s, got %v", field, err)
		}
	}

	for _, origin := range []string{"https://*.example.com", "https://example.com/path", "https://example.com/?"} {
		checkError([]config.MCPEventsTrustedCallback{{Origin: origin, Addresses: []string{"10.0.0.1"}}}, "[1].origin")
	}
	for _, pin := range []string{"fd20:ce::254", "fd00:ec2::254", "100.100.100.200", "::ffff:100.100.100.200", "8.8.8.8", "fd00::1%eth0", "10.0.0.0/8", "receiver.example"} {
		checkError([]config.MCPEventsTrustedCallback{{Origin: "https://example.com", Addresses: []string{"10.0.0.1", pin}}}, "[1].addresses[1]")
	}
	for _, tc := range []struct {
		entries []config.MCPEventsTrustedCallback
		field   string
	}{
		{[]config.MCPEventsTrustedCallback{{Origin: "https://example.com"}}, "[1].addresses"},
		{[]config.MCPEventsTrustedCallback{{Origin: "https://example.com", Addresses: []string{"10.0.0.1", "::ffff:10.0.0.1"}}}, "[1].addresses[1]"},
		{[]config.MCPEventsTrustedCallback{{Origin: "https://example.com", Addresses: []string{"10.0.0.1"}}, {Origin: "https://EXAMPLE.com:443/", Addresses: []string{"10.0.0.2"}}}, "[2].origin"},
		{[]config.MCPEventsTrustedCallback{{Origin: "https://10.0.0.1", Addresses: []string{"10.0.0.2"}}}, "[1].addresses[0]"},
	} {
		checkError(tc.entries, tc.field)
	}

}

func TestTrustedCallbackTLSAndLiteral(t *testing.T) {
	_, server := fixtureWebhook(t, func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(204) })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, tc := range []struct {
		origin, pin, callback string
	}{
		{"https://wrong.example", "10.0.0.1", "https://wrong.example/events"},
		{"https://10.0.0.1", "10.0.0.1", "https://10.0.0.1/events"},
		{"https://[fd00::1]:8443", "fd00::1", "https://[fd00::1]:8443/events"},
	} {
		policy, err := CompileTrustedCallbacks([]config.MCPEventsTrustedCallback{{Origin: tc.origin, Addresses: []string{tc.pin}}})
		if err != nil {
			t.Fatal(err)
		}
		w := NewWebhookClient(WebhookOptions{TrustedCallbacks: policy, RootCAs: roots, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			u, _ := url.Parse(tc.callback)
			port := u.Port()
			if port == "" {
				port = "443"
			}
			if address != net.JoinHostPort(tc.pin, port) {
				t.Errorf("numeric literal/port not retained: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}})
		_, err = w.Send(t.Context(), tc.callback, "evt", "sub", []byte(`{}`), []string{fixtureSecret(1, 32)})
		if safeWebhookError(err).Reason != "tls_error" {
			t.Fatalf("missing TLS rejection: %v", err)
		}
	}
}
