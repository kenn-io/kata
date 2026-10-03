package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func nonLoopbackListen(listen string) bool {
	if listen == "" {
		return false
	}
	host, _, err := net.SplitHostPort(listen)
	return err == nil && !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback()
}

func nonLoopbackWebOrigin(origin string) (exposed, https bool) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false, false
	}
	host := parsed.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return false, false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false, false
	}
	return true, strings.EqualFold(parsed.Scheme, "https")
}

func externalWebOriginAuthError() error {
	return errors.New("non-loopback web.public_origin requires an auth token or trusted proxy authentication on the browser listener")
}

func externalWebOriginTransportError() error {
	return errors.New("non-loopback web.public_origin with a non-loopback plaintext listener requires auth.trust_private_network")
}

func trustedProxyBrowserSession(
	endpoint kitdaemon.Endpoint, web config.WebConfig, auth config.AuthConfig,
) bool {
	if endpoint.Network == kitdaemon.NetworkUnix {
		address := web.Listen
		if address == "" {
			address = "127.0.0.1:0"
		}
		endpoint = kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: address}
	}
	// Compare the address as the bound TCP listener will report it. An
	// ephemeral port cannot match a configured trusted listener before binding.
	address, err := netip.ParseAddrPort(endpoint.Address)
	if err != nil || address.Port() == 0 {
		return false
	}
	endpoint.Address = net.TCPAddrFromAddrPort(address).String()
	return (daemon.WebEndpoint{Endpoint: endpoint}).AllowsTrustedProxySession(auth)
}

func prepareDaemonAuth(
	cfg *config.DaemonConfig, endpoint kitdaemon.Endpoint,
	readonly, noAutoToken bool, home string,
) error {
	auth := &cfg.Auth
	webOriginExposed, webOriginHTTPS := nonLoopbackWebOrigin(cfg.Web.PublicOrigin)
	nonLoopbackAPIListener := endpoint.Network == kitdaemon.NetworkTCP && nonLoopbackListen(endpoint.Address)
	nonLoopbackBackend := nonLoopbackAPIListener ||
		(endpoint.Network == kitdaemon.NetworkUnix && nonLoopbackListen(cfg.Web.Listen))
	exposed := nonLoopbackBackend || webOriginExposed
	secureWebOrigin := webOriginExposed && webOriginHTTPS && !nonLoopbackBackend
	trustedProxySession := trustedProxyBrowserSession(endpoint, cfg.Web, *auth)
	// Proxy actor exchange authenticates browser requests, but not ordinary API
	// clients on a non-loopback listener. Only the explicit tokenless-write
	// policy can replace the owner token for that backend.
	trustedProxySessionCanAvoidBackendToken := trustedProxySession &&
		(!nonLoopbackAPIListener || auth.AllowUnauthenticatedPrivateNetworkWrites)
	if auth.Token != "" || readonly || auth.RequireTokenIdentity ||
		trustedProxySessionCanAvoidBackendToken || !exposed {
		return nil
	}
	if auth.AllowUnauthenticatedPrivateNetworkWrites {
		if webOriginExposed {
			return externalWebOriginAuthError()
		}
		return nil
	}
	if config.EnvTruthy("KATA_AUTOSTART") {
		if webOriginExposed {
			return externalWebOriginAuthError()
		}
		return nil
	}
	// Keep the existing explicit trust requirement, even for a generated token.
	// A TLS-terminated external browser origin is separately safe for token login.
	if !auth.TrustPrivateNetwork && !secureWebOrigin {
		if webOriginExposed {
			if webOriginHTTPS && nonLoopbackBackend {
				return externalWebOriginTransportError()
			}
			return externalWebOriginAuthError()
		}
		return nil
	}
	return preparePersistedDaemonAuth(auth, home, noAutoToken, webOriginExposed)
}

func preparePersistedDaemonAuth(
	auth *config.AuthConfig, home string, noAutoToken, webOriginExposed bool,
) error {
	token, err := config.ReadPersistedAuthToken(home)
	if err != nil {
		return err
	}
	if token != "" {
		auth.Token, auth.Source = token, "persisted_file"
		return nil
	}
	if noAutoToken || (auth.AutoToken != nil && !*auth.AutoToken) {
		if webOriginExposed {
			return externalWebOriginAuthError()
		}
		return nil
	}
	token, err = mintDaemonToken(home)
	if err != nil {
		return err
	}
	auth.Token, auth.Source = token, "generated_file"
	return nil
}

// Publish the complete file atomically without replacing a concurrent winner.
func mintDaemonToken(home string) (string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret[:])
	file, err := safefileio.CreatePrivateTemp(home, ".auth-token-*")
	if err != nil {
		return "", fmt.Errorf("create auth token file: %w", err)
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.WriteString(token + "\n"); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Link(file.Name(), filepath.Join(home, "auth-token")); err != nil {
		if errors.Is(err, os.ErrExist) {
			token, readErr := config.ReadPersistedAuthToken(home)
			if readErr == nil && token == "" {
				readErr = fmt.Errorf("%w: persisted auth token disappeared during creation", config.ErrCredentialSource)
			}
			return token, readErr
		}
		return "", fmt.Errorf("publish auth token file: %w", err)
	}
	return token, nil
}
