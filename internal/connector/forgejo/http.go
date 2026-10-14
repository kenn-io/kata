package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/kata/pkg/connector"
)

const maxResponseBytes = 8 * 1024 * 1024

var providerTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

// cappedTransport bounds every provider response body, because the SDK reads
// bodies without a limit.
type cappedTransport struct{ next http.RoundTripper }

func (t cappedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil {
		response.Body = http.MaxBytesReader(nil, response.Body, maxResponseBytes)
	}
	return response, err
}

type client struct {
	origin, instance string
	api              *forgejosdk.Client
}

func failure(code, message string) *connector.Error {
	return &connector.Error{Code: code, Message: message}
}

// providerFailure maps an SDK result to a connector error without exposing the
// provider's response body, which can echo credentials or private content.
func providerFailure(response *forgejosdk.Response) *connector.Error {
	if response == nil || response.Response == nil {
		return failure("provider_error", "Forgejo request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "provider_error"
		if response.StatusCode == http.StatusNotFound {
			code = "not_found"
		}
		return failure(code, fmt.Sprintf("Forgejo returned HTTP %d", response.StatusCode))
	}
	return failure("provider_error", "Forgejo returned an invalid response")
}
func id(n int64) string { return strconv.FormatInt(n, 10) }
func numeric(s string) (int64, bool) {
	n, e := strconv.ParseInt(s, 10, 64)
	return n, e == nil && n > 0 && id(n) == s
}
func normalizedOrigin(u *url.URL) string {
	normalized := *u
	normalized.Host = strings.ToLower(normalized.Host)
	if (normalized.Scheme == "https" && normalized.Port() == "443") || (normalized.Scheme == "http" && normalized.Port() == "80") {
		normalized.Host = normalized.Hostname()
		if strings.Contains(normalized.Host, ":") {
			normalized.Host = "[" + normalized.Host + "]"
		}
	}
	normalized.Path = ""
	normalized.RawPath = ""
	return normalized.String()
}
func digest(v any) string {
	b, _ := json.Marshal(v, json.Deterministic(true))
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (h *handler) client(ctx context.Context) (*client, *connector.Error) {
	invocation, ok := connector.InvocationFromContext(ctx)
	var settings struct {
		Origin string `json:"origin"`
	}
	if !ok || json.Unmarshal(invocation.Settings, &settings, json.RejectUnknownMembers(true)) != nil {
		return nil, failure("invalid_settings", "Forgejo origin settings are invalid")
	}
	u, e := url.Parse(settings.Origin)
	if e != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(settings.Origin, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, failure("invalid_settings", "Forgejo origin settings are invalid")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return nil, failure("invalid_settings", "Forgejo origin settings are invalid")
		}
	}
	if u.Scheme == "http" {
		ip, e := netip.ParseAddr(u.Hostname())
		if e != nil || (!ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
			return nil, failure("invalid_settings", "Forgejo HTTP origin requires a literal loopback, private, link-local, or CGNAT IP address")
		}
	}
	origin := normalizedOrigin(u)
	if strings.TrimSpace(h.token) == "" {
		return nil, failure("authentication", "Forgejo token is required")
	}
	httpClient := &http.Client{Transport: cappedTransport{next: providerTransport}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// An empty version skips the SDK's /version probe and version gates.
	api, e := forgejosdk.NewClient(origin, forgejosdk.SetHTTPClient(httpClient), forgejosdk.SetToken(h.token),
		forgejosdk.SetForgejoVersion(""), forgejosdk.SetContext(ctx), forgejosdk.SetUserAgent("kata"))
	if e != nil {
		return nil, failure("provider_error", "Forgejo client could not be created")
	}
	return &client{origin: origin, instance: invocation.Instance, api: api}, nil
}

func (c *client) self() (*forgejosdk.User, *connector.Error) {
	u, response, err := c.api.GetMyUserInfo()
	if err != nil {
		return nil, providerFailure(response)
	}
	if u.ID <= 0 {
		return nil, failure("provider_error", "Forgejo returned an invalid account")
	}
	return u, nil
}
