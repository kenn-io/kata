package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonMCPIdentityHandler(ctx context.Context, store db.Storage, cfg config.MCPConfig, scope *mcpserver.Scope, endpoint kitdaemon.Endpoint, auth config.AuthConfig) (http.Handler, error) {
	newServer := func(ctx context.Context, bearer, actor string) (*sdkmcp.Server, error) {
		resolved := client.ResolvedDaemon{
			Address: endpoint.Address, Network: endpoint.Network, BaseURL: endpoint.BaseURL(),
			Token: bearer, TrustPrivateNetwork: auth.TrustPrivateNetwork, Direct: true,
			AllowInsecure: bearer == "",
		}
		if endpoint.Network == kitdaemon.NetworkUnix {
			resolved.UnixSocket = endpoint.Address
		}
		httpClient, err := httpClientForResolved(ctx, resolved)
		if err != nil {
			return nil, err
		}
		apiClient, longRunningClient, err := newMCPAPIClients(ctx, resolved, httpClient)
		if err != nil {
			return nil, err
		}
		return mcpserver.New(mcpserver.Options{
			Client: apiClient, LongRunningClient: longRunningClient,
			Scope: scope, Actor: actor, Version: version.Version,
			StartupSections: cfg.StartupSections,
		})
	}
	// Validate startup sections now, retaining the mount's existing 503 behavior.
	// This server is never served and has no credential or bootstrap authority.
	if _, err := newServer(ctx, "", "identity-token"); err != nil {
		return nil, err
	}
	type serverKey struct{}
	stream := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		server, _ := r.Context().Value(serverKey{}).(*sdkmcp.Server)
		return server
	}, &sdkmcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute, PropagateRequestCancellation: true,
		DisableLocalhostProtection: true, // daemon listener policy validates Host
	})
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
		if r.Header.Get("Mcp-Session-Id") == "" {
			info := sdkauth.TokenInfoFromContext(r.Context())
			bearer := strings.Fields(r.Header.Get("Authorization"))[1] // verified by SDK middleware
			server, err := newServer(r.Context(), bearer, info.Extra["actor"].(string))
			if err != nil {
				http.Error(w, "MCP session unavailable", http.StatusServiceUnavailable)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), serverKey{}, server))
		}
		stream.ServeHTTP(w, r)
	})
	return sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(
		http.NewCrossOriginProtection().Handler(handler)), nil
}
