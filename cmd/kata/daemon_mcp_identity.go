package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonMCPIdentityHandler(ctx context.Context, store db.Storage, cfg config.MCPConfig, scope *mcpserver.Scope, endpoint kitdaemon.Endpoint, auth config.AuthConfig) (http.Handler, error) {
	// Each legacy session or modern request owns its clients and reporter.
	// The daemon's Unix listener never times out idle connections, so a
	// session that kept its pool would hold them forever.
	newServer := func(reqCtx context.Context, bearer, actor string, events bool, admission *mcpserver.ToolAdmission, loaded *mcpserver.LoadedSections) (*sdkmcp.Server, func(), error) {
		resolved := client.ResolvedDaemon{
			Address: endpoint.Address, Network: endpoint.Network, BaseURL: endpoint.BaseURL(),
			Token: bearer, TrustPrivateNetwork: auth.TrustPrivateNetwork, Direct: true,
			AllowInsecure: bearer == "",
		}
		if endpoint.Network == kitdaemon.NetworkUnix {
			resolved.UnixSocket = endpoint.Address
		}
		httpClient, err := httpClientForResolved(reqCtx, resolved)
		if err != nil {
			return nil, nil, err
		}
		longRunningHTTPClient, err := longRunningClientForResolved(reqCtx, resolved)
		if err != nil {
			return nil, nil, err
		}
		apiClient, err := kataclient.NewWithHTTPClient(resolved.BaseURL, httpClient)
		if err != nil {
			return nil, nil, err
		}
		longRunningClient, err := kataclient.NewWithHTTPClient(resolved.BaseURL, longRunningHTTPClient)
		if err != nil {
			return nil, nil, err
		}
		// The reporter outlives the initialize request, so it uses the mount's context.
		observe, stopReporting := startMCPAgentUseReporter(ctx, apiClient, resolved)
		release := func() {
			stopReporting()
			httpClient.CloseIdleConnections()
			longRunningHTTPClient.CloseIdleConnections()
		}
		server, err := mcpserver.New(mcpserver.Options{
			Client: apiClient, LongRunningClient: longRunningClient,
			Scope: scope, Actor: actor, Version: version.Version,
			StartupSections: cfg.StartupSections,
			ObserveToolCall: observe, Events: events, Admission: admission, LoadedSections: loaded,
		})
		if err != nil {
			release()
			return nil, nil, err
		}
		return server, release, nil
	}
	// Validate startup sections now, retaining the mount's existing 503 behavior.
	// This server is never served and has no credential or bootstrap authority.
	_, release, err := newServer(ctx, "", "identity-token", false, nil, nil)
	if err != nil {
		return nil, err
	}
	release()
	type serverKey struct{}
	selectServer := func(r *http.Request) *sdkmcp.Server {
		server, _ := r.Context().Value(serverKey{}).(*sdkmcp.Server)
		return server
	}
	stream := mcpserver.NewHTTPHandler(selectServer, selectServer, mcpserver.HTTPOptions{
		DisableLocalhostProtection: true, // daemon listener policy validates Host
	})
	// Modern requests share admission and section selections only within one
	// verified credential. Legacy sessions retain their own limits and catalog.
	// No credentials, clients, reporters or servers are cached.
	type modernTokenState struct {
		admission *mcpserver.ToolAdmission
		sections  mcpserver.LoadedSections
	}
	var modernTokens sync.Map // verified token ID -> *modernTokenState

	verify := func(ctx context.Context, bearer string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if auth.Token != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(auth.Token)) == 1 {
			return nil, sdkauth.ErrInvalidToken
		}
		token, err := store.ResolveAPIToken(ctx, bearer)
		if errors.Is(err, db.ErrNotFound) {
			return nil, sdkauth.ErrInvalidToken
		}
		if err != nil {
			return nil, errors.New("token identity lookup failed")
		}
		info := &sdkauth.TokenInfo{
			// Bind to the credential, not just its actor: another token for the
			// same actor may carry a narrower grant or a different expiration.
			UserID: strconv.FormatInt(token.ID, 10),
			Extra:  map[string]any{"actor": token.Actor},
		}
		if token.ExpiresAt != nil {
			info.Expiration = *token.ExpiresAt
		}
		return info, nil
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modern := mcpserver.IsModernRequest(r)
		if modern || r.Header.Get("Mcp-Session-Id") == "" {
			info := sdkauth.TokenInfoFromContext(r.Context())
			bearer := strings.Fields(r.Header.Get("Authorization"))[1] // verified by SDK middleware
			var admission *mcpserver.ToolAdmission
			var loaded *mcpserver.LoadedSections
			if modern {
				shared, _ := modernTokens.LoadOrStore(info.UserID, &modernTokenState{admission: mcpserver.NewToolAdmission()})
				state := shared.(*modernTokenState)
				admission, loaded = state.admission, &state.sections
			}
			server, release, err := newServer(r.Context(), bearer, info.Extra["actor"].(string), modern, admission, loaded)
			if err != nil {
				http.Error(w, "MCP session unavailable", http.StatusServiceUnavailable)
				return
			}
			if modern {
				defer release()
			}
			stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), serverKey{}, server)))
			if !modern {
				releaseWhenSessionsEnd(server, release)
			}
			return
		}
		stream.ServeHTTP(w, r)
	})
	return sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(
		http.NewCrossOriginProtection().Handler(handler)), nil
}

// releaseWhenSessionsEnd frees a per-session server's clients once the
// session it served ends, or at once when initialization created none.
func releaseWhenSessionsEnd(server *sdkmcp.Server, release func()) {
	var sessions []*sdkmcp.ServerSession
	for session := range server.Sessions() {
		sessions = append(sessions, session)
	}
	go func() {
		for _, session := range sessions {
			_ = session.Wait()
		}
		release()
	}()
}
