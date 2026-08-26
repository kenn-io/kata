package tui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
)

type daemonTarget struct {
	Name             string
	Local            bool
	URL              string
	TokenEnv         string
	Implicit         bool
	resolved         client.ResolvedDaemon
	workspaceStart   string
	skipInitialScope bool
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
	readDaemonConfigForTUI         = config.ReadDaemonConfig
	ensureResolvedForTUI           = client.EnsureResolvedInWorkspace
	ensureLocalRunningTargetForTUI = client.EnsureLocalRunningTarget
	ensureResolvedNamedForTUI      = client.EnsureResolvedNamed
	normalizeRemoteURLForTUI       = func(v string, allowInsecure bool) (string, error) {
		return client.NormalizeRemoteURL(v, allowInsecure)
	}
	newHTTPClientForTUI = func(
		ctx context.Context,
		_ string,
		target daemonTarget,
		kind clientOptsKind,
	) (*http.Client, error) {
		opts := optsForKind(kind)
		opts.WorkspaceStart = target.workspaceStart
		return client.NewHTTPClientForResolved(ctx, target.resolved, opts)
	}
	bootResolveScopeForTUI         = bootResolveScope
	bootResolveScopePathFreeForTUI = bootResolveScopePathFree
	connectDaemonTargetForTUI      = connectDaemonTarget
)

func daemonTargetsFromConfig(daemons []config.CatalogDaemonConfig) []daemonTarget {
	out := make([]daemonTarget, 0, len(daemons))
	for _, d := range daemons {
		out = append(out, daemonTarget{
			Name:     d.Name,
			Local:    d.Local,
			URL:      d.URL,
			TokenEnv: d.TokenEnv,
			resolved: client.ResolvedDaemon{
				Token: d.Token, AllowInsecure: d.AllowInsecure,
			},
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

func bootDaemonConnection(ctx context.Context, opts Options) (daemonConnection, error) {
	workspaceStart, skipInitialScope, err := initialScopeDirective(opts)
	if err != nil {
		return daemonConnection{}, err
	}
	cfg, err := readDaemonConfigForTUI()
	if err != nil {
		return daemonConnection{}, err
	}
	catalog := daemonTargetsWithLaunchSelectors(
		daemonTargetsFromConfig(cfg.Daemons), opts,
	)
	targetName := cfg.ActiveDaemon
	if strings.TrimSpace(opts.DaemonName) != "" {
		targetName = strings.TrimSpace(opts.DaemonName)
	}
	target, ok := activeDaemonTarget(catalog, targetName)
	if !ok {
		if targetName != "" {
			return daemonConnection{}, fmt.Errorf("daemon %q is not in daemon catalog", targetName)
		}
		conn, err := connectImplicitDaemonTarget(ctx, workspaceStart, skipInitialScope)
		if err != nil {
			return daemonConnection{}, err
		}
		conn.catalog = catalog
		return conn, nil
	}
	target.workspaceStart = workspaceStart
	target.skipInitialScope = skipInitialScope
	conn, err := connectDaemonTargetForTUI(ctx, target)
	if err != nil {
		return daemonConnection{}, err
	}
	conn.catalog = catalog
	return conn, nil
}

func daemonTargetsWithLaunchSelectors(
	targets []daemonTarget, opts Options,
) []daemonTarget {
	out := make([]daemonTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, daemonTargetWithLaunchSelectors(target, opts))
	}
	return out
}

func daemonTargetWithLaunchSelectors(target daemonTarget, opts Options) daemonTarget {
	target.workspaceStart = strings.TrimSpace(opts.Workspace)
	target.skipInitialScope = strings.TrimSpace(opts.ProjectName) != ""
	return target
}

func connectImplicitDaemonTarget(
	ctx context.Context, workspaceStart string, skipInitialScope bool,
) (daemonConnection, error) {
	resolved, err := ensureResolvedForTUI(ctx, workspaceStart)
	if err != nil {
		return daemonConnection{}, err
	}
	target := implicitDaemonTarget(resolved.BaseURL)
	target.resolved = resolved
	target.workspaceStart = workspaceStart
	target.skipInitialScope = skipInitialScope
	return connectResolvedDaemonTarget(ctx, target, resolved.BaseURL)
}

func initialScopeDirective(opts Options) (string, bool, error) {
	qualified, err := initialIssueRefIsQualified(opts.InitialIssueRef)
	if err != nil {
		return "", false, err
	}
	skipInitialScope := qualified || strings.TrimSpace(opts.ProjectName) != ""
	return strings.TrimSpace(opts.Workspace), skipInitialScope, nil
}

func connectDaemonTarget(ctx context.Context, target daemonTarget) (daemonConnection, error) {
	resolved, err := ensureResolvedNamedForTUI(ctx, target.Name)
	if err != nil {
		return daemonConnection{}, err
	}
	target.resolved = resolved
	if target.Local {
		target.URL = ""
	} else {
		target.URL = resolved.BaseURL
	}
	return connectResolvedDaemonTarget(ctx, target, resolved.BaseURL)
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
	if endpoint == client.UnixBase {
		c.setLocalHTTPClientRefresh(localHTTPClientRefreshForTarget(endpoint, target))
	}
	var bi bootInit
	if !target.skipInitialScope {
		startPath := target.workspaceStart
		if startPath == "" {
			startPath, _ = os.Getwd()
		}
		resolveScope := bootResolveScopeForTUI
		if daemonTargetUsesRemoteFilesystem(target, endpoint) {
			resolveScope = bootResolveScopePathFreeForTUI
		}
		bi, err = resolveScope(ctx, c, startPath)
		if err != nil {
			return daemonConnection{}, err
		}
	}
	return daemonConnection{
		api:      c,
		sseHC:    sseHC,
		endpoint: endpoint,
		target:   target,
		init:     bi,
	}, nil
}

func daemonTargetUsesRemoteFilesystem(target daemonTarget, endpoint string) bool {
	if target.Local || endpoint == client.UnixBase {
		return false
	}
	if target.resolved.ConfiguredRemote() {
		return true
	}
	if !target.Implicit {
		return true
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || !ip.IsLoopback()
}

func localHTTPClientRefreshForTarget(
	endpoint string, target daemonTarget,
) func(context.Context) (*http.Client, error) {
	return func(ctx context.Context) (*http.Client, error) {
		refreshedTarget := target
		if target.Local || endpoint == client.UnixBase {
			running, err := ensureLocalRunningTargetForTUI(ctx)
			if err != nil {
				return nil, err
			}
			refreshedTarget.resolved = target.resolved.WithRunning(running)
		}
		return newHTTPClientForTUI(ctx, refreshedTarget.resolved.BaseURL, refreshedTarget, clientOptsNormal)
	}
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
