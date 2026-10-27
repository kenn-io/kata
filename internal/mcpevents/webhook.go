package mcpevents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	webhookTimeout           = 10 * time.Second
	webhookEnvelopeLimit     = 256 * 1024
	webhookVerificationLimit = 64 * 1024
	webhookRetryAfterLimit   = 5 * time.Minute
)

// WebhookError deliberately retains no callback URL, secret, response body or
// wrapped network error. Reasons are safe for persisted delivery diagnostics.
type WebhookError struct {
	Reason string
	cause  error
}

func (e *WebhookError) Error() string { return "webhook: " + e.Reason }
func (e *WebhookError) Unwrap() error { return e.cause }

// WebhookReceipt reports receiver acceptance, not completion of an inbox task.
// HTTP rejection policy and durable retries belong to the delivery service.
type WebhookReceipt struct {
	StatusCode int
	RetryAfter time.Duration
}

// WebhookOptions provides explicit network dependencies for embedded clients
// and isolated receivers. Every resolved address is still checked before DialContext.
// A dialer must connect only to the supplied numeric destination. RootCAs is
// optional; system roots and normal hostname verification are the default.
type WebhookOptions struct {
	Resolve     func(context.Context, string, string) ([]netip.Addr, error)
	DialContext func(context.Context, string, string) (net.Conn, error)
	RootCAs     *x509.CertPool
	Now         func() time.Time
}

// WebhookClient is independent of daemon clients, bearer tokens and environment
// proxies. Callers may share a client between concurrent subscriptions.
type WebhookClient struct {
	client  *http.Client
	resolve func(context.Context, string, string) ([]netip.Addr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
	now     func() time.Time
}

// NewWebhookClient creates a credential-free, SSRF-checked webhook transport.
func NewWebhookClient(options ...WebhookOptions) *WebhookClient {
	w := &WebhookClient{resolve: net.DefaultResolver.LookupNetIP, dial: (&net.Dialer{Timeout: webhookTimeout}).DialContext, now: time.Now}
	var roots *x509.CertPool
	for _, option := range options {
		if option.Resolve != nil {
			w.resolve = option.Resolve
		}
		if option.DialContext != nil {
			w.dial = option.DialContext
		}
		if option.Now != nil {
			w.now = option.Now
		}
		if option.RootCAs != nil {
			roots = option.RootCAs.Clone()
		}
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            w.dialPublic,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    webhookTimeout,
		ResponseHeaderTimeout:  webhookTimeout,
		MaxResponseHeaderBytes: webhookVerificationLimit,
		DisableKeepAlives:      true,
		TLSNextProto:           make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	w.client = &http.Client{
		Transport:     transport,
		Timeout:       webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return w
}

// ValidateWebhookCallback validates syntax and literal destinations without DNS
// or other network activity. Resolved addresses are always checked again at dial.
func ValidateWebhookCallback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || u.Opaque != "" {
		return &WebhookError{Reason: "invalid_callback"}
	}
	if strings.HasSuffix(u.Host, ":") {
		return &WebhookError{Reason: "invalid_callback"}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return &WebhookError{Reason: "invalid_callback"}
		}
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if !publicWebhookIP(ip) {
			return &WebhookError{Reason: "invalid_callback"}
		}
	} else if !validWebhookHostname(u.Hostname()) {
		return &WebhookError{Reason: "invalid_callback"}
	}
	return nil
}

func validWebhookHostname(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, suffix := range []string{"localhost", "local", "internal", "home.arpa"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	// Numeric and hexadecimal TLDs can conceal alternative IPv4 spellings.
	last := labels[len(labels)-1]
	if _, err := strconv.ParseUint(last, 10, 64); err == nil || strings.HasPrefix(last, "0x") {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

var webhookProhibitedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

var webhookGlobalIPv6 = netip.MustParsePrefix("2000::/3")
var webhookProhibitedIPv6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // protocol assignments, including Teredo and benchmarking
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("2002::/16"),     // deprecated 6to4 encapsulation
	netip.MustParsePrefix("3fff::/20"),     // documentation
}

func publicWebhookIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	prefixes := webhookProhibitedIPv4
	if ip.Is6() {
		// Only globally allocated IPv6 unicast is eligible. This excludes NAT64,
		// IPv4-compatible, local, multicast and reserved address space.
		if !webhookGlobalIPv6.Contains(ip) {
			return false
		}
		prefixes = webhookProhibitedIPv6
	}
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (w *WebhookClient) dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	// net/http may detach a dial from its request cancellation. Bound the
	// resolver and socket independently of the request's ten-second timeout.
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &WebhookError{Reason: "invalid_callback"}
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		if !validWebhookHostname(host) {
			return nil, &WebhookError{Reason: "invalid_callback"}
		}
		addresses, err = w.resolve(ctx, "ip", host)
		if err != nil {
			return nil, safeWebhookError(err)
		}
	}
	if len(addresses) == 0 {
		return nil, &WebhookError{Reason: "dns_error"}
	}
	// Validate the entire answer before trying any address: a mixed answer fails
	// closed even if its public address could successfully connect.
	for _, ip := range addresses {
		if !publicWebhookIP(ip) {
			return nil, &WebhookError{Reason: "invalid_callback"}
		}
	}
	var last error
	for _, ip := range addresses {
		conn, err := w.dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, safeWebhookError(last)
}

// ValidateWebhookSecret requires canonical padded Standard Webhooks base64 and
// a 24–64 byte decoded key. It performs no network activity.
func ValidateWebhookSecret(secret string) error { _, err := decodeWebhookSecret(secret); return err }

func decodeWebhookSecret(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") || len(secret) > 94 {
		return nil, &WebhookError{Reason: "invalid_secret"}
	}
	encoded := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) < 24 || len(key) > 64 || base64.StdEncoding.EncodeToString(key) != encoded {
		return nil, &WebhookError{Reason: "invalid_secret"}
	}
	return key, nil
}

func safeWebhookError(err error) *WebhookError {
	if safe, ok := errors.AsType[*WebhookError](err); ok {
		return safe
	}
	if errors.Is(err, ErrRevoked) {
		return &WebhookError{Reason: "principal_revoked", cause: ErrRevoked}
	}
	if errors.Is(err, ErrUnavailable) {
		return &WebhookError{Reason: "source_unavailable", cause: ErrUnavailable}
	}
	var netError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netError) && netError.Timeout() {
		return &WebhookError{Reason: "timeout"}
	}
	if errors.Is(err, context.Canceled) {
		return &WebhookError{Reason: "canceled"}
	}
	var certError *tls.CertificateVerificationError
	var recordError tls.RecordHeaderError
	if errors.As(err, &certError) || errors.As(err, &recordError) {
		return &WebhookError{Reason: "tls_error"}
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return &WebhookError{Reason: "dns_error"}
	}
	return &WebhookError{Reason: "connection_failed"}
}

func validWebhookHeader(value string) bool {
	if value == "" || len(value) > 1024 {
		return false
	}
	for _, c := range value {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (w *WebhookClient) request(ctx context.Context, callback, eventID, subscriptionID string, body []byte, keys []string) (*http.Response, error) {
	if err := ValidateWebhookCallback(callback); err != nil {
		return nil, err
	}
	if len(body) > webhookEnvelopeLimit {
		return nil, &WebhookError{Reason: "payload_too_large"}
	}
	if !validWebhookHeader(eventID) || !validWebhookHeader(subscriptionID) {
		return nil, &WebhookError{Reason: "invalid_identifier"}
	}
	if len(keys) < 1 || len(keys) > 2 {
		return nil, &WebhookError{Reason: "invalid_secret"}
	}
	decoded := make([][]byte, 0, len(keys))
	for _, key := range keys {
		value, err := decodeWebhookSecret(key)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return nil, &WebhookError{Reason: "invalid_callback"}
	}
	timestamp := strconv.FormatInt(w.now().Unix(), 10)
	signatures := make([]string, 0, len(decoded))
	for _, key := range decoded {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(eventID + "." + timestamp + "."))
		_, _ = mac.Write(body)
		signatures = append(signatures, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", eventID)
	req.Header.Set("Webhook-Timestamp", timestamp)
	req.Header.Set("Webhook-Signature", strings.Join(signatures, " "))
	req.Header.Set("X-MCP-Subscription-Id", subscriptionID)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, safeWebhookError(err)
	}
	return resp, nil
}

// Verify authenticates the receiver using one fresh signed challenge. Activation,
// verification caching and durable key rotation are the caller's responsibility.
func (w *WebhookClient) Verify(ctx context.Context, callback, subscriptionID, secret string) error {
	if err := ValidateWebhookCallback(callback); err != nil {
		return err
	}
	if err := ValidateWebhookSecret(secret); err != nil {
		return err
	}
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return &WebhookError{Reason: "challenge_failed"}
	}
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes)
	body, _ := json.Marshal(struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}{"verification", challenge})
	resp, err := w.request(ctx, callback, "msg_verification_"+challenge, subscriptionID, body, []string{secret})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &WebhookError{Reason: "challenge_http_error"}
	}
	response, err := io.ReadAll(io.LimitReader(resp.Body, webhookVerificationLimit+1))
	if err != nil {
		return safeWebhookError(err)
	}
	if len(response) > webhookVerificationLimit {
		return &WebhookError{Reason: "challenge_response_too_large"}
	}
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(response, &echo) != nil || !hmac.Equal([]byte(echo.Challenge), []byte(challenge)) {
		return &WebhookError{Reason: "challenge_failed"}
	}
	return nil
}

// Send signs the exact persisted body. Retries retain body and eventID, while
// each call creates a fresh signing timestamp and signatures for supplied keys.
func (w *WebhookClient) Send(ctx context.Context, callback, eventID, subscriptionID string, body []byte, keys []string) (WebhookReceipt, error) {
	resp, err := w.request(ctx, callback, eventID, subscriptionID, body, keys)
	if err != nil {
		return WebhookReceipt{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return WebhookReceipt{StatusCode: resp.StatusCode, RetryAfter: webhookRetryAfter(resp.Header.Get("Retry-After"), w.now())}, nil
}

func webhookRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	var delay time.Duration
	if strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(webhookRetryAfterLimit/time.Second) {
			return webhookRetryAfterLimit
		}
		delay = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(value); err == nil {
		delay = date.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > webhookRetryAfterLimit {
		return webhookRetryAfterLimit
	}
	return delay
}
