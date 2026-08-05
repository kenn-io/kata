package daemon

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
)

// ListenerKind identifies which trust boundary accepted a request.
type ListenerKind string

const (
	// ListenerSocket preserves the owner-local Unix-socket browser-Origin ban.
	ListenerSocket ListenerKind = "socket"
	// ListenerBrowser is a dedicated browser-facing TCP listener.
	ListenerBrowser ListenerKind = "browser"
	// ListenerSharedTCP serves both ordinary daemon clients and browsers.
	ListenerSharedTCP ListenerKind = "shared-tcp"
)

// ListenerPolicy applies transport-specific browser boundaries outside the
// shared route and authentication stack.
type ListenerPolicy struct {
	Kind                  ListenerKind
	Origin                string
	BackendAuthority      string
	AllowedHosts          []string
	RequireBrowserSession bool
	AllowLocalSession     bool
	WebAuthentication     string
}

// ApplyListenerPolicy wraps a shared handler for one listener.
func ApplyListenerPolicy(next http.Handler, policy ListenerPolicy) (http.Handler, error) {
	switch policy.Kind {
	case ListenerSocket:
		return withCSRFGuards(restrictLocalSession(next, policy)), nil
	case ListenerBrowser, ListenerSharedTCP:
		origin, err := url.Parse(policy.Origin)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" {
			return nil, errors.New("browser listener policy requires an HTTP or HTTPS origin")
		}
		allowedHosts := make(map[string]struct{})
		addAllowedAuthority(allowedHosts, origin.Host)
		if policy.Kind == ListenerSharedTCP && policy.BackendAuthority != "" {
			backend, err := config.NormalizeWebHostAuthority(policy.BackendAuthority)
			if err != nil {
				return nil, errors.New("shared listener policy requires a valid backend authority")
			}
			addAllowedAuthority(allowedHosts, backend)
		}
		if policy.Kind == ListenerSharedTCP {
			for _, authority := range policy.AllowedHosts {
				host, err := config.NormalizeWebHostAuthority(authority)
				if err != nil {
					return nil, errors.New("shared listener policy requires valid allowed hosts")
				}
				addAllowedAuthority(allowedHosts, host)
			}
		}
		jsonHandler := requireJSONMutation(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := allowedHosts[strings.ToLower(r.Host)]; !ok || r.Host == "" {
				api.WriteEnvelope(w, http.StatusBadRequest, "host_invalid", "Host header does not match listener authority")
				return
			}
			if policy.Kind == ListenerBrowser || isBrowserRequest(r) {
				originHeader := r.Header.Get("Origin")
				if isMutation(r.Method) && originHeader != policy.Origin {
					api.WriteEnvelope(w, http.StatusForbidden, "origin_forbidden", "Origin header does not match browser origin")
					return
				}
			}
			if isLocalSessionRequest(r) && !directLoopbackSessionAllowed(r, policy) {
				http.NotFound(w, r)
				return
			}
			jsonHandler.ServeHTTP(w, r)
		}), nil
	default:
		return nil, errors.New("unknown listener policy")
	}
}

func restrictLocalSession(next http.Handler, policy ListenerPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLocalSessionRequest(r) && !directLoopbackSessionAllowed(r, policy) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalSessionRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/api/v1/ui/session/local"
}

func directLoopbackSessionAllowed(r *http.Request, policy ListenerPolicy) bool {
	if !policy.AllowLocalSession {
		return false
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		if r.Header.Get(header) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func addAllowedAuthority(allowed map[string]struct{}, authority string) {
	allowed[strings.ToLower(authority)] = struct{}{}
	host, port := authority, ""
	if parsedHost, parsedPort, err := net.SplitHostPort(authority); err == nil {
		host, port = parsedHost, parsedPort
	}
	add := func(alias string) {
		if port != "" {
			alias = net.JoinHostPort(alias, port)
		}
		allowed[strings.ToLower(alias)] = struct{}{}
	}
	switch {
	case strings.EqualFold(host, "localhost"):
		add("127.0.0.1")
		add("::1")
	case net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback():
		add("localhost")
	case host == "0.0.0.0" || host == "::":
		add("127.0.0.1")
		add("localhost")
		add("::1")
	}
}

func isBrowserRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/v1/ui/") ||
		r.Header.Get("Origin") != "" || r.Header.Get(webSessionHeader) != "" {
		return true
	}
	for _, cookie := range r.Cookies() {
		if strings.HasPrefix(cookie.Name, "kata_session_") ||
			strings.HasPrefix(cookie.Name, "__Host-kata_session_") {
			return true
		}
	}
	return !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/openapi.yaml"
}

// CheckWebStartup applies the daemon's existing remote-access policy to a
// configured browser bind and additionally refuses public literals/hostnames.
func CheckWebStartup(listen string, auth config.AuthConfig, insecureReadonly bool) error {
	if err := ValidateNonPublicAddress(listen); err != nil {
		return err
	}
	return CheckAuthStartup(listen, auth, insecureReadonly)
}
