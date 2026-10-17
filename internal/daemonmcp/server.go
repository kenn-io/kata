// Package daemonmcp mounts the shared Kata tools on an authoritative daemon.
package daemonmcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
	kitdaemon "go.kenn.io/kit/daemon"
)

// Service owns a fixed startup scope and a shared tool admission budget.
type Service struct {
	store       db.Storage
	cfg         config.MCPConfig
	proxyHeader string
	scope       *mcpserver.Scope
	api         http.Handler
	admission   sdkmcp.Middleware
}

// New never invokes api during startup, allowing it to close over the daemon
// server that will subsequently mount this service. No client config is read.
func New(ctx context.Context, cfg *config.DaemonConfig, endpoint kitdaemon.Endpoint, readonly bool, store db.Storage, api http.Handler) (*Service, error) {
	if err := cfg.MCP.Validate(); err != nil {
		return nil, err
	}
	if !cfg.MCP.Enabled {
		return nil, nil
	}
	if endpoint.Network != kitdaemon.NetworkTCP || (!remoteAuthority(endpoint.Address) && !remoteOrigin(cfg.Web.PublicOrigin)) {
		return nil, errors.New("mcp.enabled requires a server TCP listener or non-loopback web.public_origin")
	}
	if readonly || (cfg.Auth.Token == "" && !cfg.Auth.RequireTokenIdentity) {
		return nil, errors.New("mcp.enabled requires daemon bearer authentication and writable server mode")
	}
	if !cfg.Auth.RequireTokenIdentity && cfg.MCP.Actor == "" {
		return nil, errors.New("mcp.actor is required with a static daemon token")
	}
	if len(cfg.Federation.Projects) != 0 {
		return nil, errors.New("mcp.enabled is not available on a configured federation spoke")
	}
	s := &Service{store: store, cfg: cfg.MCP, proxyHeader: cfg.Auth.Proxy.TrustedActorHeader,
		scope: mcpserver.NewAllScope(), api: api, admission: mcpserver.NewToolAdmissionMiddleware(nil)}
	ok, err := s.Available(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("mcp.enabled is not available on a federation spoke")
	}
	if len(cfg.MCP.Projects) != 0 {
		catalog, err := store.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		projects := make([]mcpserver.ProjectIdentity, 0, len(cfg.MCP.Projects))
		for _, name := range cfg.MCP.Projects {
			found := false
			for _, project := range catalog {
				if project.Name == name {
					projects = append(projects, mcpserver.ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("mcp.projects: project %q not found", name)
			}
		}
		s.scope, err = mcpserver.NewAllowlistScope(projects)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func remoteAuthority(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	return err == nil && !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback()
}

func remoteOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), "localhost") && !net.ParseIP(u.Hostname()).IsLoopback()
}

// Available fails closed after any project becomes a spoke, including a
// disabled binding or a relay. Mixed-role daemons cannot expose replicas.
func (s *Service) Available(ctx context.Context) (bool, error) {
	bindings, err := s.store.ListFederationBindings(ctx)
	if err != nil {
		return false, err
	}
	for _, binding := range bindings {
		if binding.Role == db.FederationRoleSpoke {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Available(r.Context())
	if err != nil {
		http.Error(w, "MCP eligibility unavailable", 503)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	principal, authenticated := daemon.PrincipalFromContext(r.Context())
	if !authenticated {
		http.Error(w, "bearer authentication required", 401)
		return
	}
	actor := principal.Actor
	if principal.Kind == daemon.PrincipalStaticToken {
		actor = s.cfg.Actor
	}
	if actor == "" {
		http.Error(w, "authenticated actor required", 403)
		return
	}
	headers := http.Header{"Authorization": r.Header.Values("Authorization")}
	if s.proxyHeader != "" {
		headers[http.CanonicalHeaderKey(s.proxyHeader)] = r.Header.Values(s.proxyHeader)
	}
	transport := requestTransport{handler: s.api, headers: headers}
	client, err := kataclient.NewWithHTTPClient("http://mcp.internal", &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: rejectRedirect})
	if err != nil {
		http.Error(w, "MCP client unavailable", 500)
		return
	}
	longClient, err := kataclient.NewWithHTTPClient("http://mcp.internal", &http.Client{Transport: transport, CheckRedirect: rejectRedirect})
	if err != nil {
		http.Error(w, "MCP client unavailable", 500)
		return
	}
	server, err := mcpserver.New(mcpserver.Options{Client: client, LongRunningClient: longClient, Scope: s.scope,
		Actor: actor, Version: version.Version, PreloadTools: true, ToolAdmission: s.admission})
	if err != nil {
		http.Error(w, "MCP server unavailable", 500)
		return
	}
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, MaxRequestBodyBytes: 8 << 20,
	})
	http.NewCrossOriginProtection().Handler(handler).ServeHTTP(privateResponseWriter{w}, r)
}

func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// The SDK sets its own cache header. Enforce the daemon policy when headers
// reach the wire, including JSON responses and protocol errors.
type privateResponseWriter struct{ http.ResponseWriter }

func (w privateResponseWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (w privateResponseWriter) Write(body []byte) (int, error) {
	w.Header().Set("Cache-Control", "private, no-store")
	return w.ResponseWriter.Write(body)
}

func (w privateResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
