package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
)

const (
	webDaemonHeaderName   = "X-Kata-Web-Daemon"
	webDaemonProxyPrefix  = "/api/v1/ui/proxy"
	webDaemonHealthTTL    = 5 * time.Second
	webDaemonProbeTimeout = 2 * time.Second
	webDaemonProxyTimeout = 30 * time.Second
)

var (
	errWebDaemonRedirectForbidden = errors.New("daemon redirect forbidden")
	errWebDaemonUpgradeRequired   = errors.New("daemon UI contract upgrade required")
)

type webDaemonResponse struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Default bool   `json:"default"`
	Auth    string `json:"auth"`
	Health  string `json:"health"`
	Hint    string `json:"hint,omitempty"`
}

type webDaemonRosterResponse struct {
	Daemons []webDaemonResponse `json:"daemons"`
}

type resolvedWebDaemon struct {
	id, baseURL, token   string
	local                bool
	credentialConfigured bool
	allowInsecure        bool
}

type webDaemonHealthEntry struct {
	state   string
	expires time.Time
}

type webDaemonInflightProbe struct {
	done  chan struct{}
	state string
}

type webDaemonGateway struct {
	catalog             []config.CatalogDaemonConfig
	active              string
	localMux            *http.ServeMux
	sessions            *WebSessionManager
	insecureReadonly    bool
	trustPrivateNetwork bool

	mu       sync.Mutex
	health   map[string]webDaemonHealthEntry
	inflight map[string]*webDaemonInflightProbe
	proxies  map[string]http.Handler
}

func registerWebDaemonHandlers(mux *http.ServeMux, cfg ServerConfig) {
	gateway := &webDaemonGateway{
		catalog: append([]config.CatalogDaemonConfig(nil), cfg.WebDaemons...),
		active:  strings.TrimSpace(cfg.ActiveWebDaemon), localMux: mux,
		sessions: cfg.WebSessions, insecureReadonly: cfg.InsecureReadonly,
		trustPrivateNetwork: cfg.Auth.TrustPrivateNetwork,
		health:              make(map[string]webDaemonHealthEntry),
		inflight:            make(map[string]*webDaemonInflightProbe),
		proxies:             make(map[string]http.Handler),
	}
	mux.HandleFunc(http.MethodGet+" /api/v1/ui/daemons", gateway.list)
	mux.Handle(webDaemonProxyPrefix+"/", http.StripPrefix(webDaemonProxyPrefix, gateway))
}

func (g *webDaemonGateway) list(w http.ResponseWriter, r *http.Request) {
	catalog := g.effectiveCatalog()
	if insecureReadonlyRequest(r.Context()) {
		visible := make([]config.CatalogDaemonConfig, 0, len(catalog))
		for _, configured := range catalog {
			if !webDaemonCredentialsConfigured(configured) {
				visible = append(visible, configured)
			}
		}
		catalog = visible
	}
	resolved := make([]resolvedWebDaemon, len(catalog))
	states := make([]string, len(catalog))
	var wg sync.WaitGroup
	wg.Add(len(catalog))
	for i := range catalog {
		i := i
		go func() {
			defer wg.Done()
			resolved[i] = resolveWebDaemon(catalog[i])
			states[i] = g.daemonHealth(r.Context(), resolved[i])
		}()
	}
	wg.Wait()

	defaultID := g.defaultID(catalog)
	out := webDaemonRosterResponse{Daemons: make([]webDaemonResponse, 0, len(catalog))}
	for i, configured := range catalog {
		d := resolved[i]
		auth := "none"
		if d.credentialConfigured {
			auth = "token"
		}
		hint := ""
		switch states[i] {
		case "down":
			hint = "daemon is not reachable"
		case "upgrade_required":
			hint = "daemon does not support the Kata web UI"
		}
		out.Daemons = append(out.Daemons, webDaemonResponse{
			ID: configured.Name, URL: redactWebDaemonURL(d.baseURL),
			Default: configured.Name == defaultID, Auth: auth,
			Health: states[i], Hint: hint,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		slog.Debug("write web daemon roster", "err", err)
	}
}

func (g *webDaemonGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", webDaemonHeaderName)
	if !webDaemonProxyRequestAllowed(r, r.URL.Path) {
		writeWebDaemonError(w, http.StatusForbidden, "web_daemon_operation_forbidden")
		return
	}
	policy := g.sourcePolicy(r.Context())
	readOnlyProjectRequest, rejected := classifyWebDaemonProjectRequest(w, r)
	if rejected {
		return
	}
	if isMutation(r.Method) && !readOnlyProjectRequest && !policy.writable {
		writeWebDaemonError(w, http.StatusForbidden, "read_only")
		return
	}
	d, err := g.selectDaemon(r.Header.Get(webDaemonHeaderName))
	if err != nil {
		writeWebDaemonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Path == pathEventsStreamPath && (!d.local || policy.insecureReadonly) {
		writeWebDaemonError(w, http.StatusForbidden, "web_daemon_stream_forbidden")
		return
	}
	if !d.local && policy.insecureReadonly && d.credentialConfigured {
		writeWebDaemonError(w, http.StatusForbidden, "web_daemon_readonly_target_forbidden")
		return
	}
	stripWebDaemonBrowserCredentials(r.Header)
	if d.local {
		g.localMux.ServeHTTP(w, r)
		return
	}
	proxy, err := g.proxy(d)
	if err != nil {
		writeWebDaemonError(w, http.StatusBadRequest, "invalid_daemon_target")
		return
	}
	if upstreamETag, ok := decodeWebDaemonETag(r.Header.Get("If-None-Match"), policy); ok {
		r.Header.Set("If-None-Match", upstreamETag)
	}
	if webDaemonCapabilityPath(r.URL.Path) {
		r.Header.Del("Accept-Encoding")
	}
	proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webDaemonSourcePolicyKey{}, policy)))
}

func classifyWebDaemonProjectRequest(w http.ResponseWriter, r *http.Request) (readOnly, rejected bool) {
	if r.Method != http.MethodPost || r.Body == nil ||
		(r.URL.Path != "/api/v1/projects" && r.URL.Path != "/api/v1/projects/resolve") {
		return false, false
	}
	const maxProjectRequest = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProjectRequest+1))
	if err != nil {
		writeWebDaemonError(w, http.StatusBadRequest, "invalid_project_request")
		return false, true
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	if len(body) > maxProjectRequest {
		writeWebDaemonError(w, http.StatusRequestEntityTooLarge, "project_request_too_large")
		return false, true
	}
	if r.URL.Path == "/api/v1/projects/resolve" {
		var input api.ResolveProjectRequest
		if json.Unmarshal(body, &input.Body) != nil {
			return false, false
		}
		if strings.TrimSpace(input.Body.Name) != "" && input.Body.Alias == nil &&
			strings.TrimSpace(input.Body.StartPath) == "" {
			return true, false
		}
		writeWebDaemonError(w, http.StatusForbidden, "web_local_operation_forbidden")
		return false, true
	}
	var input api.InitProjectRequest
	if json.Unmarshal(body, &input.Body) != nil {
		return false, false
	}
	if webProjectInitFieldsAllowed(&input) {
		return false, false
	}
	writeWebDaemonError(w, http.StatusForbidden, "web_local_operation_forbidden")
	return false, true
}

type webDaemonSourcePolicyKey struct{}

type webDaemonSourcePolicy struct {
	writable         bool
	insecureReadonly bool
}

func (g *webDaemonGateway) sourcePolicy(ctx context.Context) webDaemonSourcePolicy {
	principal, _ := PrincipalFromContext(ctx)
	policy := webDaemonSourcePolicy{
		writable: !g.insecureReadonly && principalAllowsWebWrites(principal),
	}
	if g.sessions != nil {
		policy.writable = policy.writable && g.sessions.CanWrite(principal)
	}
	policy.insecureReadonly = insecureReadonlyRequest(ctx)
	if policy.insecureReadonly {
		policy.writable = false
	}
	return policy
}

func (g *webDaemonGateway) effectiveCatalog() []config.CatalogDaemonConfig {
	if len(g.catalog) > 0 {
		return g.catalog
	}
	return []config.CatalogDaemonConfig{{Name: "local", Local: true}}
}

func (g *webDaemonGateway) defaultID(catalog []config.CatalogDaemonConfig) string {
	if g.active != "" {
		for _, d := range catalog {
			if d.Name == g.active {
				return g.active
			}
		}
	}
	for _, d := range catalog {
		if d.Local {
			return d.Name
		}
	}
	if len(catalog) > 0 {
		return catalog[0].Name
	}
	return ""
}

func (g *webDaemonGateway) selectDaemon(requested string) (resolvedWebDaemon, error) {
	catalog := g.effectiveCatalog()
	id := strings.TrimSpace(requested)
	if id == "" {
		id = g.defaultID(catalog)
	}
	for _, d := range catalog {
		if d.Name == id {
			resolved := resolveWebDaemon(d)
			if !resolved.local && resolved.baseURL == "" {
				return resolvedWebDaemon{}, errors.New("invalid_daemon_target")
			}
			return resolved, nil
		}
	}
	return resolvedWebDaemon{}, errors.New("unknown_daemon")
}

func resolveWebDaemon(d config.CatalogDaemonConfig) resolvedWebDaemon {
	resolved := resolvedWebDaemon{
		id: d.Name, local: d.Local, credentialConfigured: webDaemonCredentialsConfigured(d),
		allowInsecure: d.AllowInsecure,
	}
	if d.Local {
		return resolved
	}
	trimmed := strings.TrimSpace(d.URL)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Opaque != "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(trimmed, "#") {
		return resolved
	}
	canonical, err := config.CanonicalHTTPOrigin(trimmed)
	if err != nil {
		return resolved
	}
	parsed, err = url.Parse(canonical)
	if err != nil {
		return resolved
	}
	if parsed.Scheme == "http" && !d.AllowInsecure {
		if err := ValidateNonPublicAddress(net.JoinHostPort(parsed.Hostname(), "1")); err != nil {
			return resolved
		}
	}
	resolved.baseURL = canonical
	resolved.token = d.Token
	if resolved.token == "" && d.TokenEnv != "" {
		resolved.token = strings.TrimSpace(os.Getenv(d.TokenEnv))
	}
	return resolved
}

func webDaemonCredentialsConfigured(d config.CatalogDaemonConfig) bool {
	return strings.TrimSpace(d.Token) != "" || strings.TrimSpace(d.TokenEnv) != ""
}

func redactWebDaemonURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func (g *webDaemonGateway) daemonHealth(ctx context.Context, d resolvedWebDaemon) string {
	if d.local {
		return "connected"
	}
	if d.baseURL == "" {
		return "down"
	}
	key := strings.Join([]string{d.id, d.baseURL, d.token}, "\x00")
	g.mu.Lock()
	if cached, ok := g.health[key]; ok && time.Now().Before(cached.expires) {
		g.mu.Unlock()
		return cached.state
	}
	if probe, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		select {
		case <-probe.done:
			return probe.state
		case <-ctx.Done():
			return "down"
		}
	}
	probe := &webDaemonInflightProbe{done: make(chan struct{})}
	g.inflight[key] = probe
	g.mu.Unlock()

	state := probeWebDaemon(ctx, d, g.trustPrivateNetwork)
	g.mu.Lock()
	probe.state = state
	g.health[key] = webDaemonHealthEntry{state: state, expires: time.Now().Add(webDaemonHealthTTL)}
	delete(g.inflight, key)
	close(probe.done)
	g.mu.Unlock()
	return state
}

func probeWebDaemon(parent context.Context, d resolvedWebDaemon, trustPrivateNetwork bool) string {
	ctx, cancel := context.WithTimeout(parent, webDaemonProbeTimeout)
	defer cancel()
	target, err := url.JoinPath(d.baseURL, "api/v1/instance")
	if err != nil {
		return "down"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "down"
	}
	transport, err := webDaemonBearerTransport(d, trustPrivateNetwork)
	if err != nil {
		return "down"
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return "down"
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		var instance struct {
			WebUIContractVersion string `json:"web_ui_contract_version"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&instance); err != nil ||
			instance.WebUIContractVersion != api.UISnapshotContractVersion {
			return "upgrade_required"
		}
		return "connected"
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		return "auth_required"
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		return "down"
	}
}

func (g *webDaemonGateway) proxy(d resolvedWebDaemon) (http.Handler, error) {
	key := strings.Join([]string{d.id, d.baseURL, d.token}, "\x00")
	g.mu.Lock()
	if cached := g.proxies[key]; cached != nil {
		g.mu.Unlock()
		return cached, nil
	}
	g.mu.Unlock()
	target, err := url.Parse(d.baseURL)
	if err != nil || target.Host == "" {
		return nil, errors.New("invalid daemon target")
	}
	transport, err := webDaemonBearerTransport(d, g.trustPrivateNetwork)
	if err != nil {
		return nil, err
	}
	reverse := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
			request.Out.Header = webDaemonOutboundHeaders(request.In.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Del(WebOriginHeader)
			response.Header.Del(WebAuthenticationHeader)
			if response.StatusCode >= http.StatusMultipleChoices &&
				response.StatusCode < http.StatusBadRequest && response.StatusCode != http.StatusNotModified {
				return errWebDaemonRedirectForbidden
			}
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				body := []byte(`{"error":{"code":"daemon_auth_required"}}`)
				_ = response.Body.Close()
				response.StatusCode = http.StatusBadGateway
				response.Status = "502 Bad Gateway"
				response.Body = io.NopCloser(bytes.NewReader(body))
				response.ContentLength = int64(len(body))
				response.Header.Del("WWW-Authenticate")
				response.Header.Set("Content-Type", "application/json")
				response.Header.Set("Content-Length", strconv.Itoa(len(body)))
				return nil
			}
			if webDaemonCapabilityPath(response.Request.URL.Path) &&
				(response.StatusCode == http.StatusOK || response.StatusCode == http.StatusNotModified) {
				policy, _ := response.Request.Context().Value(webDaemonSourcePolicyKey{}).(webDaemonSourcePolicy)
				if upstreamETag := response.Header.Get("ETag"); upstreamETag != "" {
					response.Header.Set("ETag", encodeWebDaemonETag(upstreamETag, policy))
				}
				if response.StatusCode == http.StatusOK {
					if err := restrictWebDaemonCapabilities(response, policy); err != nil {
						return err
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("web daemon proxy failed", "daemon", d.id, "target", redactWebDaemonURL(d.baseURL), "err", err)
			if errors.Is(err, errWebDaemonRedirectForbidden) {
				writeWebDaemonError(w, http.StatusBadGateway, "daemon_redirect_forbidden")
				return
			}
			if errors.Is(err, errWebDaemonUpgradeRequired) {
				writeWebDaemonError(w, http.StatusBadGateway, "daemon_upgrade_required")
				return
			}
			writeWebDaemonError(w, http.StatusBadGateway, "daemon_unreachable")
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), webDaemonProxyTimeout)
		defer cancel()
		reverse.ServeHTTP(w, r.WithContext(ctx))
	})
	g.mu.Lock()
	if cached := g.proxies[key]; cached != nil {
		g.mu.Unlock()
		return cached, nil
	}
	g.proxies[key] = handler
	g.mu.Unlock()
	return handler, nil
}

func webDaemonBearerTransport(d resolvedWebDaemon, trustPrivateNetwork bool) (http.RoundTripper, error) {
	if d.token == "" {
		return http.DefaultTransport, nil
	}
	policy := config.BearerPolicy{
		TrustPrivateNetwork: trustPrivateNetwork, AllowInsecurePlaintext: d.allowInsecure,
	}
	var origin string
	var err error
	if d.allowInsecure {
		origin, err = config.BearerOriginForBaseURLAllowInsecure(d.baseURL)
	} else {
		origin, err = config.BearerOriginForBaseURLWithTrust(d.baseURL, trustPrivateNetwork)
	}
	if err != nil {
		return nil, err
	}
	return config.BearerTransportWithPolicy(http.DefaultTransport, d.token, origin, policy), nil
}

func webDaemonCapabilityPath(path string) bool {
	return path == "/api/v1/ui/snapshot" || path == "/api/v1/ui/references"
}

func restrictWebDaemonCapabilities(response *http.Response, policy webDaemonSourcePolicy) error {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read daemon capability response: %w", err)
	}
	_ = response.Body.Close()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode daemon capability response: %w", err)
	}
	if envelope == nil {
		return errors.New("decode daemon capability response: expected object")
	}
	var contractVersion string
	if err := json.Unmarshal(envelope["contract_version"], &contractVersion); err != nil ||
		contractVersion != api.UISnapshotContractVersion {
		return errWebDaemonUpgradeRequired
	}
	var capabilities api.UICapabilities
	if err := json.Unmarshal(envelope["capabilities"], &capabilities); err != nil {
		return fmt.Errorf("decode daemon capabilities: %w", err)
	}
	capabilities.Writable = capabilities.Writable && policy.writable
	capabilities.Updates = "poll"
	encodedCapabilities, err := json.Marshal(capabilities)
	if err != nil {
		return fmt.Errorf("encode daemon capabilities: %w", err)
	}
	envelope["capabilities"] = encodedCapabilities
	body, err = json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode daemon capability response: %w", err)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func encodeWebDaemonETag(upstream string, policy webDaemonSourcePolicy) string {
	mode := "ro"
	if policy.writable {
		mode = "rw"
	}
	return `"kata-daemon-` + mode + `-` + base64.RawURLEncoding.EncodeToString([]byte(upstream)) + `"`
}

func decodeWebDaemonETag(value string, policy webDaemonSourcePolicy) (string, bool) {
	mode := "ro"
	if policy.writable {
		mode = "rw"
	}
	prefix := `"kata-daemon-` + mode + `-`
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`)
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

func stripWebDaemonBrowserCredentials(headers http.Header) {
	for _, name := range []string{
		"Authorization", "Cookie", "Origin", webSessionHeader, webCSRFHeader,
		webDaemonHeaderName, "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	} {
		headers.Del(name)
	}
}

func webDaemonOutboundHeaders(in http.Header) http.Header {
	out := make(http.Header)
	for _, name := range []string{
		"Accept", "Content-Type", "Idempotency-Key", "If-Match", "If-None-Match",
	} {
		if values := in.Values(name); len(values) > 0 {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}

func webDaemonProxyRequestAllowed(r *http.Request, path string) bool {
	if strings.HasPrefix(path, webDaemonProxyPrefix) {
		return false
	}
	if (path == pathPing || path == pathHealth) && r.Method == http.MethodGet {
		return true
	}
	if path == "/api/v1/projects/resolve" && r.Method == http.MethodPost {
		return true
	}
	request := &http.Request{Method: r.Method, URL: &url.URL{Path: path, RawQuery: r.URL.RawQuery}}
	return webLocalSPARequestAllowed(request)
}

func writeWebDaemonError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	payload := struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}{}
	payload.Error.Code = code
	_ = json.NewEncoder(w).Encode(payload)
}
