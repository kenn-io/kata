package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpHTTPShutdownTimeout = 10 * time.Second

func resolveMCPHTTPToken(address, tokenEnv string, trustPrivateNetwork bool) (string, error) {
	address = strings.TrimSpace(address)
	tokenEnv = strings.TrimSpace(tokenEnv)
	if address == "" {
		if tokenEnv != "" {
			return "", errors.New("--http-token-env requires --http")
		}
		if trustPrivateNetwork {
			return "", errors.New("--trust-private-network requires --http")
		}
		return "", nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid --http address %q: expected host:port", address)
	}

	var token string
	if tokenEnv != "" {
		token = strings.TrimSpace(os.Getenv(tokenEnv))
		if token == "" {
			return "", fmt.Errorf("--http-token-env %q is unset or empty", tokenEnv)
		}
	}
	if token == "" {
		return "", errors.New("--http listeners require --http-token-env")
	}
	if mcpHTTPHostRequiresToken(host) {
		if !trustPrivateNetwork {
			return "", errors.New("non-loopback --http listeners require --trust-private-network")
		}
		if !mcpHTTPHostAllowsTrustedPrivateNetwork(host) {
			return "", fmt.Errorf("non-loopback --http address %q must use a non-public IP or wildcard bind", address)
		}
	}
	return token, nil
}

func mcpHTTPHostRequiresToken(host string) bool {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}

func mcpHTTPHostAllowsTrustedPrivateNetwork(host string) bool {
	if host == "" {
		return true
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	address = address.Unmap()
	return address.IsUnspecified() || address.IsPrivate() || address.IsLinkLocalUnicast() ||
		mcpHTTPCGNATPrefix.Contains(address)
}

var mcpHTTPCGNATPrefix = netip.MustParsePrefix("100.64.0.0/10")

func serveMCPHTTP(
	ctx context.Context,
	status io.Writer,
	address string,
	token string,
	server *sdkmcp.Server,
) error {
	listener, err := net.Listen("tcp", strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("listen for MCP HTTP: %w", err)
	}
	defer func() { _ = listener.Close() }()

	var mcpHandler http.Handler = sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{
			SessionTimeout:               30 * time.Minute,
			PropagateRequestCancellation: true,
		},
	)
	mcpHandler = http.NewCrossOriginProtection().Handler(mcpHandler)
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireMCPHTTPBearer(token, mcpHandler))
	mux.HandleFunc("/healthz", serveMCPHTTPHealth)
	var handler http.Handler = mux
	reportedAuthority := listener.Addr().String()
	configuredHost, _, splitErr := net.SplitHostPort(strings.TrimSpace(address))
	if splitErr != nil {
		return fmt.Errorf("resolve MCP HTTP authority: %w", splitErr)
	}
	if !mcpHTTPHostRequiresToken(configuredHost) {
		reportedAuthority, err = mcpHTTPConfiguredAuthority(address, listener.Addr())
		if err != nil {
			return err
		}
		handler = requireMCPHTTPHost(reportedAuthority, handler)
	}

	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return serveContext },
	}

	if _, err := fmt.Fprintf(status, "kata mcp: listening on http://%s/mcp\n", reportedAuthority); err != nil {
		return fmt.Errorf("report MCP HTTP listener: %w", err)
	}
	return runMCPHTTPServer(serveContext, httpServer, listener)
}

func runMCPHTTPServer(ctx context.Context, server *http.Server, listener net.Listener) error {
	shutdownSignal, signalShutdown := context.WithCancel(ctx)
	defer signalShutdown()
	shutdownDone := make(chan error, 1)
	go func() {
		<-shutdownSignal.Done()
		shutdownContext, stopShutdown := context.WithTimeout(
			context.WithoutCancel(shutdownSignal), mcpHTTPShutdownTimeout,
		)
		defer stopShutdown()
		shutdownDone <- server.Shutdown(shutdownContext)
	}()

	serveErr := server.Serve(listener)
	signalShutdown()
	shutdownErr := <-shutdownDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		if shutdownErr != nil {
			return fmt.Errorf("serve MCP HTTP: %w (shutdown also failed: %v)", serveErr, shutdownErr)
		}
		return fmt.Errorf("serve MCP HTTP: %w", serveErr)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shut down MCP HTTP: %w", shutdownErr)
	}
	return nil
}

func mcpHTTPConfiguredAuthority(address string, listenerAddress net.Addr) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", fmt.Errorf("resolve configured MCP HTTP authority: %w", err)
	}
	_, port, err := net.SplitHostPort(listenerAddress.String())
	if err != nil {
		return "", fmt.Errorf("resolve listening MCP HTTP authority: %w", err)
	}
	return net.JoinHostPort(host, port), nil
}

func requireMCPHTTPHost(authority string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !sameMCPHTTPAuthority(request.Host, authority) {
			http.Error(writer, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func sameMCPHTTPAuthority(left, right string) bool {
	leftHost, leftPort, ok := splitMCPHTTPAuthority(left)
	if !ok {
		return false
	}
	rightHost, rightPort, ok := splitMCPHTTPAuthority(right)
	if !ok || leftPort != rightPort {
		return false
	}
	leftIP, leftErr := netip.ParseAddr(leftHost)
	rightIP, rightErr := netip.ParseAddr(rightHost)
	if leftErr == nil || rightErr == nil {
		return leftErr == nil && rightErr == nil && leftIP.Unmap() == rightIP.Unmap()
	}
	return strings.EqualFold(leftHost, rightHost)
}

func splitMCPHTTPAuthority(authority string) (string, string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err == nil {
		return host, port, host != "" && port != ""
	}
	if authority == "" || strings.Contains(authority, ":") {
		return "", "", false
	}
	return authority, "80", true
}

func requireMCPHTTPBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		presented, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func serveMCPHTTPHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = io.WriteString(writer, "ok\n")
	}
}
