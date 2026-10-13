package forgejo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

var providerTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

type client struct {
	origin, token, instance string
	http                    *http.Client
}
type user struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"full_name"`
}

func failure(code, message string) *connector.Error {
	return &connector.Error{Code: code, Message: message}
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
	return &client{origin: origin, token: h.token, instance: invocation.Instance, http: &http.Client{Transport: providerTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *client) request(ctx context.Context, method, path string, payload, output any) *connector.Error {
	var body io.Reader
	if payload != nil {
		raw, e := json.Marshal(payload)
		if e != nil {
			return failure("invalid_request", "Forgejo request is invalid")
		}
		body = bytes.NewReader(raw)
	}
	request, e := http.NewRequestWithContext(ctx, method, c.origin+"/api/v1"+path, body)
	if e != nil {
		return failure("provider_error", "Forgejo request could not be sent")
	}
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, e := c.http.Do(request)
	if e != nil {
		return failure("provider_error", "Forgejo request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "provider_error"
		if response.StatusCode == http.StatusNotFound {
			code = "not_found"
		}
		return failure(code, fmt.Sprintf("Forgejo returned HTTP %d", response.StatusCode))
	}
	if output != nil {
		raw, e := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
		if e != nil || len(raw) > 8*1024*1024 || json.Unmarshal(raw, output) != nil {
			return failure("provider_error", "Forgejo returned an invalid response")
		}
	}
	return nil
}
func (c *client) self(ctx context.Context) (user, *connector.Error) {
	var u user
	if err := c.request(ctx, "GET", "/user", nil, &u); err != nil {
		return u, err
	}
	if u.ID <= 0 {
		return u, failure("provider_error", "Forgejo returned an invalid account")
	}
	return u, nil
}
