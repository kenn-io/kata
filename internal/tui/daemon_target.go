package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
)

type daemonTarget struct {
	Name          string
	Local         bool
	URL           string
	Token         string
	TokenEnv      string
	AllowInsecure bool
	Implicit      bool
}

type daemonConnection struct {
	api      *Client
	sseHC    *http.Client
	endpoint string
	target   daemonTarget
	catalog  []daemonTarget
	init     bootInit
}

type clientOptsKind int

const (
	clientOptsNormal clientOptsKind = iota
	clientOptsSSE
)

var (
	readDaemonConfigForTUI   = config.ReadDaemonConfig
	ensureRunningForTUI      = client.EnsureRunning
	ensureLocalRunningForTUI = client.EnsureLocalRunning
	normalizeRemoteURLForTUI = func(v string, allowInsecure bool) (string, error) {
		return client.NormalizeRemoteURL(v, allowInsecure)
	}
	probeRemoteForTUI = func(ctx context.Context, base string) bool {
		return client.Ping(ctx, &http.Client{Timeout: remoteProbeTimeout}, base)
	}
	newHTTPClientForTUI = func(
		ctx context.Context,
		endpoint string,
		target daemonTarget,
		kind clientOptsKind,
	) (*http.Client, error) {
		if (target.Local || target.Implicit) && target.Token == "" {
			return client.NewHTTPClient(ctx, endpoint, optsForKind(kind))
		}
		return client.NewHTTPClientForTarget(ctx, endpoint,
			client.TargetAuth{Token: target.Token, AllowInsecure: target.AllowInsecure},
			optsForKind(kind))
	}
	bootResolveScopeForTUI    = bootResolveScope
	connectDaemonTargetForTUI = connectDaemonTarget
)

func daemonTargetsFromConfig(daemons []config.CatalogDaemonConfig) []daemonTarget {
	out := make([]daemonTarget, 0, len(daemons))
	for _, d := range daemons {
		out = append(out, daemonTarget{
			Name:          d.Name,
			Local:         d.Local,
			URL:           d.URL,
			Token:         d.Token,
			TokenEnv:      d.TokenEnv,
			AllowInsecure: d.AllowInsecure,
		})
	}
	return out
}

func activeDaemonTarget(targets []daemonTarget, active string) (daemonTarget, bool) {
	if active == "" {
		return daemonTarget{}, false
	}
	for _, target := range targets {
		if target.Name == active {
			return target, true
		}
	}
	return daemonTarget{}, false
}

func bootDaemonConnection(ctx context.Context, _ Options) (daemonConnection, error) {
	cfg, err := readDaemonConfigForTUI()
	if err != nil {
		return daemonConnection{}, err
	}
	catalog := daemonTargetsFromConfig(cfg.Daemons)
	target, ok := activeDaemonTarget(catalog, cfg.ActiveDaemon)
	if !ok {
		conn, err := connectImplicitDaemonTarget(ctx)
		if err != nil {
			return daemonConnection{}, err
		}
		conn.catalog = catalog
		return conn, nil
	}
	conn, err := connectDaemonTarget(ctx, target)
	if err != nil {
		return daemonConnection{}, err
	}
	conn.catalog = catalog
	return conn, nil
}

func connectImplicitDaemonTarget(ctx context.Context) (daemonConnection, error) {
	endpoint, err := ensureRunningForTUI(ctx)
	if err != nil {
		return daemonConnection{}, err
	}
	target := implicitDaemonTarget(endpoint)
	return connectResolvedDaemonTarget(ctx, target, endpoint)
}

func connectDaemonTarget(ctx context.Context, target daemonTarget) (daemonConnection, error) {
	var err error
	target, err = resolveDaemonTargetToken(target)
	if err != nil {
		return daemonConnection{}, err
	}
	endpoint, err := resolveDaemonEndpoint(ctx, target)
	if err != nil {
		return daemonConnection{}, err
	}
	return connectResolvedDaemonTarget(ctx, target, endpoint)
}

func resolveDaemonTargetToken(target daemonTarget) (daemonTarget, error) {
	if target.TokenEnv == "" {
		return target, nil
	}
	token := strings.TrimSpace(os.Getenv(target.TokenEnv))
	if token == "" {
		return target, fmt.Errorf("daemon %q: token_env %q is unset or empty",
			daemonTargetDisplay(target), target.TokenEnv)
	}
	target.Token = token
	return target, nil
}

func connectResolvedDaemonTarget(ctx context.Context, target daemonTarget, endpoint string) (daemonConnection, error) {
	hc, err := newHTTPClientForTUI(ctx, endpoint, target, clientOptsNormal)
	if err != nil {
		return daemonConnection{}, err
	}
	sseHC, err := newHTTPClientForTUI(ctx, endpoint, target, clientOptsSSE)
	if err != nil {
		return daemonConnection{}, err
	}
	c := NewClient(endpoint, hc)
	cwd, _ := os.Getwd()
	bi, err := bootResolveScopeForTUI(ctx, c, cwd)
	if err != nil {
		return daemonConnection{}, err
	}
	return daemonConnection{
		api:      c,
		sseHC:    sseHC,
		endpoint: endpoint,
		target:   resolvedDaemonTarget(target, endpoint),
		init:     bi,
	}, nil
}

func resolveDaemonEndpoint(ctx context.Context, target daemonTarget) (string, error) {
	if target.Local {
		return ensureLocalRunningForTUI(ctx)
	}
	endpoint, err := normalizeRemoteURLForTUI(target.URL, target.AllowInsecure)
	if err != nil {
		return "", err
	}
	if !probeRemoteForTUI(ctx, endpoint) {
		return "", fmt.Errorf("%w: %s", client.ErrRemoteUnavailable, endpoint)
	}
	return endpoint, nil
}

func resolvedDaemonTarget(target daemonTarget, endpoint string) daemonTarget {
	target.URL = endpoint
	if target.Local {
		target.URL = ""
	}
	return target
}

func implicitDaemonTarget(endpoint string) daemonTarget {
	if endpoint == client.UnixBase {
		return daemonTarget{Local: true, Implicit: true}
	}
	return daemonTarget{URL: endpoint, Implicit: true}
}

func daemonTargetDisplay(target daemonTarget) string {
	if target.Name != "" {
		return target.Name
	}
	if target.Local {
		return "local"
	}
	u, err := url.Parse(target.URL)
	if err == nil && u.Host != "" {
		return u.Host
	}
	if target.URL != "" {
		return target.URL
	}
	return "local"
}

func optsForKind(kind clientOptsKind) client.Opts {
	if kind == clientOptsSSE {
		return client.Opts{ResponseHeaderTimeout: client.SSEHandshakeTimeout}
	}
	return client.Opts{Timeout: defaultHTTPTimeout}
}
