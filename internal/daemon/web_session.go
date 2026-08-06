package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const (
	webSessionHeader = "X-Kata-Web-Session"
	webCSRFHeader    = "X-Kata-CSRF"
	webSessionTTL    = 24 * time.Hour
	// WebOriginHeader advertises the canonical browser origin on session errors.
	WebOriginHeader = "X-Kata-Web-Origin"
	// WebAuthenticationHeader advertises how the canonical browser origin authenticates.
	WebAuthenticationHeader = "X-Kata-Web-Authentication"
)

var (
	// ErrWebSessionInvalid reports a missing or mismatched cookie/session pair.
	ErrWebSessionInvalid = errors.New("web session invalid")
	// ErrWebCSRFInvalid reports a missing or mismatched mutation CSRF value.
	ErrWebCSRFInvalid = errors.New("web CSRF invalid")
	// ErrWebLoginInvalid reports a token that cannot create a browser session.
	ErrWebLoginInvalid = errors.New("web login invalid")
)

// WebSessionManagerConfig configures process-local browser authority.
type WebSessionManagerConfig struct {
	Origin       string
	OriginStable bool
	InstanceID   string
	Clock        func() time.Time
	Entropy      io.Reader
	Writable     bool
	Updates      string
	Auth         config.AuthConfig
	DB           db.Storage
}

// IssuedWebSession contains the cookie and header credentials returned once.
type IssuedWebSession struct {
	Cookie     string
	Session    string
	CSRF       string
	ReturnPath string
	Principal  Principal
	Writable   bool
	Updates    string
}

type webSessionState struct {
	csrfHash  [32]byte
	principal Principal
	expiresAt time.Time
}

// WebSessionManager owns process-local browser sessions.
type WebSessionManager struct {
	mu           sync.Mutex
	origin       string
	originStable bool
	instanceID   string
	cookieName   string
	cookieValue  string
	cookieHash   [32]byte
	secureCookie bool
	clock        func() time.Time
	entropy      io.Reader
	writable     bool
	updates      string
	auth         config.AuthConfig
	db           db.Storage
	sessions     map[[32]byte]webSessionState
}

// NewWebSessionManager constructs isolated browser authority for one origin.
func NewWebSessionManager(cfg WebSessionManagerConfig) (*WebSessionManager, error) {
	origin, err := url.Parse(cfg.Origin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" {
		return nil, errors.New("web session manager requires an HTTP or HTTPS origin")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("web session manager requires an instance ID")
	}
	for _, r := range cfg.InstanceID {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return nil, errors.New("web session manager instance ID must be alphanumeric")
		}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	entropy := cfg.Entropy
	if entropy == nil {
		entropy = randReader{}
	}
	updates := cfg.Updates
	if updates == "" {
		updates = "poll"
	}
	cookieName := "kata_session_" + cfg.InstanceID
	secure := origin.Scheme == "https"
	if secure {
		cookieName = "__Host-" + cookieName
	}
	manager := &WebSessionManager{
		origin:       cfg.Origin,
		originStable: cfg.OriginStable,
		instanceID:   cfg.InstanceID,
		cookieName:   cookieName,
		secureCookie: secure,
		clock:        clock,
		entropy:      entropy,
		writable:     cfg.Writable,
		updates:      updates,
		auth:         cfg.Auth,
		db:           cfg.DB,
		sessions:     make(map[[32]byte]webSessionState),
	}
	cookieValue, err := manager.randomValue()
	if err != nil {
		return nil, err
	}
	manager.cookieValue = cookieValue
	manager.cookieHash = sha256.Sum256([]byte(cookieValue))
	return manager, nil
}

// randReader keeps crypto/rand behind io.Reader without exposing it in tests.
type randReader struct{}

func (randReader) Read(p []byte) (int, error) { return rand.Read(p) }

// IssueSession generates independent per-tab session-header and CSRF values.
// The ambient cookie is shared by this manager so one tab cannot overwrite
// another tab's cookie while the header still isolates their authority.
func (m *WebSessionManager) IssueSession(principal Principal, returnPath string) (IssuedWebSession, error) {
	normalized, err := normalizeWebReturnPath(returnPath)
	if err != nil {
		return IssuedWebSession{}, err
	}
	session, err := m.randomValue()
	if err != nil {
		return IssuedWebSession{}, err
	}
	csrf, err := m.randomValue()
	if err != nil {
		return IssuedWebSession{}, err
	}
	sessionKey := sha256.Sum256([]byte(session))
	m.mu.Lock()
	m.sessions[sessionKey] = webSessionState{
		csrfHash:  sha256.Sum256([]byte(csrf)),
		principal: principal,
		expiresAt: m.clock().Add(webSessionTTL),
	}
	m.mu.Unlock()
	return IssuedWebSession{
		Cookie: m.cookieValue, Session: session, CSRF: csrf, ReturnPath: normalized,
		Principal: principal, Writable: m.CanWrite(principal), Updates: m.updates,
	}, nil
}

// Authenticate validates a cookie and session-header pair.
func (m *WebSessionManager) Authenticate(ctx context.Context, cookie, session string) (Principal, error) {
	key := sha256.Sum256([]byte(session))
	m.mu.Lock()
	state, ok := m.sessions[key]
	m.mu.Unlock()
	presented := sha256.Sum256([]byte(cookie))
	if !ok || subtle.ConstantTimeCompare(presented[:], m.cookieHash[:]) != 1 ||
		!m.clock().Before(state.expiresAt) {
		if ok {
			m.Logout(session)
		}
		return Principal{}, ErrWebSessionInvalid
	}
	if state.principal.Kind == PrincipalDBToken && !m.databaseTokenActive(ctx, state.principal.TokenID) {
		m.Logout(session)
		return Principal{}, ErrWebSessionInvalid
	}
	return state.principal, nil
}

func (m *WebSessionManager) databaseTokenActive(ctx context.Context, tokenID int64) bool {
	if m.db == nil || tokenID == 0 {
		return false
	}
	tokens, err := m.db.ListAPITokens(ctx)
	if err != nil {
		return false
	}
	for _, token := range tokens {
		if token.ID == tokenID {
			return token.RevokedAt == nil
		}
	}
	return false
}

// CheckCSRF validates a CSRF value against an existing session.
func (m *WebSessionManager) CheckCSRF(session, csrf string) error {
	key := sha256.Sum256([]byte(session))
	m.mu.Lock()
	state, ok := m.sessions[key]
	m.mu.Unlock()
	presented := sha256.Sum256([]byte(csrf))
	if !ok || subtle.ConstantTimeCompare(presented[:], state.csrfHash[:]) != 1 {
		return ErrWebCSRFInvalid
	}
	return nil
}

// Logout invalidates the session identified by its header value.
func (m *WebSessionManager) Logout(session string) {
	m.mu.Lock()
	delete(m.sessions, sha256.Sum256([]byte(session)))
	m.mu.Unlock()
}

// Login validates an existing daemon token and creates a browser session.
func (m *WebSessionManager) Login(ctx context.Context, token, returnPath string) (IssuedWebSession, error) {
	principal, err := m.authenticateLogin(ctx, token)
	if err != nil {
		return IssuedWebSession{}, err
	}
	return m.IssueSession(principal, returnPath)
}

func (m *WebSessionManager) authenticateLogin(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrWebLoginInvalid
	}
	if m.auth.Token != "" && constantTimeStringEqual(token, m.auth.Token) {
		kind := PrincipalStaticToken
		if m.auth.RequireTokenIdentity {
			kind = PrincipalBootstrap
		}
		return Principal{Kind: kind}, nil
	}
	if !m.auth.RequireTokenIdentity || m.db == nil {
		return Principal{}, ErrWebLoginInvalid
	}
	tokenRow, err := m.db.ResolveAPIToken(ctx, token)
	if err != nil {
		return Principal{}, ErrWebLoginInvalid
	}
	return principalFromAPIToken(tokenRow), nil
}

func constantTimeStringEqual(a, b string) bool {
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:]) == 1
}

func (m *WebSessionManager) randomValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(m.entropy, raw); err != nil {
		return "", fmt.Errorf("generate web credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func normalizeWebReturnPath(value string) (string, error) {
	if value == "" {
		return "/kata", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "", errors.New("web return path must be a same-origin absolute path")
	}
	return parsed.RequestURI(), nil
}

// CookieName returns the instance-scoped browser cookie name.
func (m *WebSessionManager) CookieName() string { return m.cookieName }

// Cookie builds the browser cookie with origin-appropriate attributes.
func (m *WebSessionManager) Cookie(value string) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure must remain false for browser-supported HTTP loopback origins.
		Name: m.cookieName, Value: value, Path: "/", HttpOnly: true,
		Secure: m.secureCookie, SameSite: http.SameSiteStrictMode,
	}
}

// Writable reports whether this manager may authorize browser mutations.
func (m *WebSessionManager) Writable() bool { return m.writable }

// CanWrite reports whether listener policy and the authenticated browser
// principal both permit ordinary mutations.
func (m *WebSessionManager) CanWrite(principal Principal) bool {
	return m.writable && principalAllowsWebWrites(principal)
}

func principalAllowsWebWrites(principal Principal) bool {
	switch principal.Kind {
	case PrincipalBootstrap, PrincipalTrustedProxyAbsent:
		return false
	default:
		return true
	}
}

// Updates reports whether the browser should subscribe with SSE or poll.
func (m *WebSessionManager) Updates() string { return m.updates }

// Origin returns the canonical browser origin for this daemon.
func (m *WebSessionManager) Origin() string { return m.origin }

// OriginStable reports whether browser origin-local state survives restarts.
func (m *WebSessionManager) OriginStable() bool { return m.originStable }

type webSessionContextKey struct{}
type webSessionRevalidationContextKey struct{}

func withWebSession(ctx context.Context, principal Principal) context.Context {
	ctx = context.WithValue(ctx, webSessionContextKey{}, true)
	if principal.Kind != "" {
		ctx = WithPrincipal(ctx, principal)
	}
	return ctx
}

func webSessionAuthenticated(ctx context.Context) bool {
	value, _ := ctx.Value(webSessionContextKey{}).(bool)
	return value
}

func withWebSessionRevalidation(
	ctx context.Context,
	manager *WebSessionManager,
	cookie string,
	session string,
) context.Context {
	return context.WithValue(ctx, webSessionRevalidationContextKey{}, func(checkCtx context.Context) error {
		_, err := manager.Authenticate(checkCtx, cookie, session)
		return err
	})
}

func revalidateBrowserSession(ctx context.Context) error {
	revalidate, _ := ctx.Value(webSessionRevalidationContextKey{}).(func(context.Context) error)
	if revalidate == nil {
		return nil
	}
	return revalidate(ctx)
}

func revalidateSSEAuthority(ctx context.Context) error {
	if err := revalidateHostAccess(ctx); err != nil {
		return err
	}
	return revalidateBrowserSession(ctx)
}

func requireBrowserSession(manager *WebSessionManager, policy ListenerPolicy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !browserSessionRequired(r, policy, manager) {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(manager.CookieName())
		if err != nil {
			writeWebSessionError(w, http.StatusUnauthorized, "web_session_required", manager.Origin(), policy)
			return
		}
		sessionValue := r.Header.Get(webSessionHeader)
		principal, err := manager.Authenticate(r.Context(), cookie.Value, sessionValue)
		if err != nil {
			writeWebSessionError(w, http.StatusUnauthorized, "web_session_required", manager.Origin(), policy)
			return
		}
		if principal.Kind == PrincipalWebLocal && !webLocalSPARequestAllowed(r) {
			writeWebSessionError(w, http.StatusForbidden, "web_local_operation_forbidden", manager.Origin(), policy)
			return
		}
		if isMutation(r.Method) {
			isLogout := r.Method == http.MethodDelete && r.URL.Path == "/api/v1/ui/session"
			if !isLogout && !manager.CanWrite(principal) {
				writeWebSessionError(w, http.StatusForbidden, "read_only", manager.Origin(), policy)
				return
			}
			if err := manager.CheckCSRF(sessionValue, r.Header.Get(webCSRFHeader)); err != nil {
				writeWebSessionError(w, http.StatusForbidden, "csrf_invalid", manager.Origin(), policy)
				return
			}
		}
		ctx := withWebSession(r.Context(), principal)
		ctx = withWebSessionRevalidation(ctx, manager, cookie.Value, sessionValue)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Add("Vary", "Cookie")
		w.Header().Add("Vary", webSessionHeader)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func webLocalSPARequestAllowed(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/ui/snapshot", "/api/v1/ui/references", "/api/v1/ui/daemons", pathEventsStreamPath:
		return r.Method == http.MethodGet
	case "/api/v1/ui/session":
		return r.Method == http.MethodDelete
	case "/api/v1/projects":
		return r.Method == http.MethodPost
	}
	if strings.HasPrefix(r.URL.Path, webDaemonProxyPrefix+"/") {
		innerPath := strings.TrimPrefix(r.URL.Path, webDaemonProxyPrefix)
		return webDaemonProxyRequestAllowed(r, innerPath)
	}

	const projectPrefix = "/api/v1/projects/"
	if !strings.HasPrefix(r.URL.Path, projectPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, projectPrefix), "/")
	if len(parts) < 2 {
		return false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return false
	}

	switch parts[1] {
	case "metadata":
		return len(parts) == 2 && r.Method == http.MethodPost
	case "recurrences":
		return (len(parts) == 2 && r.Method == http.MethodPost) ||
			(len(parts) == 3 && (r.Method == http.MethodPatch || r.Method == http.MethodDelete))
	case "issues":
		return webLocalIssueRequestAllowed(r, parts[2:])
	default:
		return false
	}
}

func webLocalIssueRequestAllowed(r *http.Request, parts []string) bool {
	if len(parts) == 0 {
		return r.Method == http.MethodPost
	}
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			_, includeDeleted := r.URL.Query()["include_deleted"]
			return !includeDeleted
		}
		return r.Method == http.MethodPatch
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "comments", "labels", "links", "metadata":
			return r.Method == http.MethodPost
		case "actions":
			return false
		}
	}
	if len(parts) == 3 {
		if parts[1] == "labels" {
			return r.Method == http.MethodDelete
		}
		if parts[1] == "actions" && r.Method == http.MethodPost {
			switch parts[2] {
			case "assign", "close", "move", "priority", "reopen", "unassign":
				return true
			}
		}
	}
	return false
}

func browserSessionRequired(r *http.Request, policy ListenerPolicy, manager *WebSessionManager) bool {
	if !policy.RequireBrowserSession || !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		return false
	}
	if r.URL.Path == pathPing || r.URL.Path == pathHealth || isWebSessionBootstrapRequest(r) {
		return false
	}
	if !manager.Writable() && r.Method == http.MethodGet && r.URL.Path != pathEventsStreamPath {
		return r.Header.Get(webSessionHeader) != ""
	}
	if policy.Kind == ListenerSharedTCP {
		if manager.auth.Token != "" && r.Header.Get(authHeader) != "" &&
			r.Header.Get("Origin") == "" && r.Header.Get(webSessionHeader) == "" &&
			r.Header.Get("Cookie") == "" {
			return false
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/ui/") {
			_, cookieErr := r.Cookie(manager.CookieName())
			unmarked := r.Header.Get("Origin") == "" && r.Header.Get(webSessionHeader) == "" &&
				errors.Is(cookieErr, http.ErrNoCookie)
			if manager.auth.Token == "" && unmarked && directBackendRequest(r, policy,
				manager.auth.AllowUnauthenticatedPrivateNetworkWrites) {
				return false
			}
			return true
		}
		if r.Header.Get("Origin") != "" || r.Header.Get(webSessionHeader) != "" {
			return true
		}
		if _, err := r.Cookie(manager.CookieName()); err == nil {
			return true
		}
		if manager.auth.Token != "" && r.Header.Get(authHeader) != "" {
			return false
		}
		return !directBackendRequest(r, policy,
			manager.auth.AllowUnauthenticatedPrivateNetworkWrites)
	}
	return true
}

func isWebSessionBootstrapRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/ui/session/login", "/api/v1/ui/session/local", "/api/v1/ui/session/proxy":
		return true
	default:
		return false
	}
}

func directBackendRequest(r *http.Request, policy ListenerPolicy, allowPrivateNetwork bool) bool {
	backendAuthorities := make(map[string]struct{})
	if authority, err := config.NormalizeWebHostAuthority(policy.BackendAuthority); err == nil {
		addAllowedAuthority(backendAuthorities, authority)
	}
	for _, configured := range policy.AllowedHosts {
		if authority, err := config.NormalizeWebHostAuthority(configured); err == nil {
			addAllowedAuthority(backendAuthorities, authority)
		}
	}
	if _, ok := backendAuthorities[strings.ToLower(r.Host)]; !ok || r.Host == "" {
		return false
	}
	if allowPrivateNetwork {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeWebSessionError(w http.ResponseWriter, status int, code, origin string, policy ListenerPolicy) {
	if origin != "" {
		w.Header().Set(WebOriginHeader, origin)
	}
	authentication := policy.WebAuthentication
	if authentication == "" && policy.AllowLocalSession {
		authentication = "loopback"
	}
	if authentication != "" {
		w.Header().Set(WebAuthenticationHeader, authentication)
	}
	api.WriteEnvelope(w, status, code, "browser session authorization failed")
}
