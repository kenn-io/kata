package tui

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestDaemonTargetsFromConfigIncludesConfiguredEntries(t *testing.T) {
	cfg := config.TUIConfig{
		ActiveDaemon: "shared",
		Daemons: []config.TUIDaemonConfig{
			{Name: "local", Local: true},
			{Name: "shared", URL: "http://100.64.0.5:7777", Token: "tok", AllowInsecure: true},
		},
	}

	targets := daemonTargetsFromConfig(cfg)

	require.Len(t, targets, 2)
	assert.Equal(t, daemonTarget{Name: "local", Local: true}, targets[0])
	assert.Equal(t, daemonTarget{
		Name:          "shared",
		URL:           "http://100.64.0.5:7777",
		Token:         "tok",
		AllowInsecure: true,
	}, targets[1])
}

func TestActiveDaemonTargetUsesConfiguredActive(t *testing.T) {
	targets := []daemonTarget{
		{Name: "local", Local: true},
		{Name: "shared", URL: "https://kata.example.test"},
	}

	target, ok := activeDaemonTarget(targets, "shared")

	require.True(t, ok)
	assert.Equal(t, "shared", target.Name)
}

func TestDaemonTargetDisplayPrefersName(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{Name: "shared", URL: "https://kata.example.test:9443"})

	assert.Equal(t, "shared", got)
}

func TestDaemonTargetDisplayFallsBackToHostPort(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{URL: "https://kata.example.test:9443"})

	assert.Equal(t, "kata.example.test:9443", got)
}

func TestDaemonTargetDisplayLocalFallback(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{Local: true})

	assert.Equal(t, "local", got)
}

func TestConnectDaemonTargetLocalUsesEnsureRunningPath(t *testing.T) {
	oldEnsure := ensureRunningForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		ensureRunningForTUI = oldEnsure
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	var ensured bool
	ensureRunningForTUI = func(context.Context) (string, error) {
		ensured = true
		return "http://kata.invalid", nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := connectDaemonTarget(context.Background(), daemonTarget{Name: "local", Local: true})

	require.NoError(t, err)
	assert.True(t, ensured, "local daemon must use existing EnsureRunning path")
	assert.Equal(t, "http://kata.invalid", conn.endpoint)
	assert.Equal(t, "local", daemonTargetDisplay(conn.target))
	assert.Equal(t, viewEmpty, conn.init.view)
}

func TestConnectDaemonTargetRemoteUsesPerDaemonAuth(t *testing.T) {
	oldNormalize := normalizeRemoteURLForTUI
	oldProbe := probeRemoteForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		normalizeRemoteURLForTUI = oldNormalize
		probeRemoteForTUI = oldProbe
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	target := daemonTarget{Name: "shared", URL: "http://daemon.internal:7777", Token: "tok", AllowInsecure: true}
	var gotNormal, gotSSE daemonTarget
	normalizeRemoteURLForTUI = func(v string, allowInsecure bool) (string, error) {
		require.Equal(t, target.URL, v)
		require.True(t, allowInsecure)
		return "http://daemon.internal:7777", nil
	}
	probeRemoteForTUI = func(context.Context, string) bool { return true }
	newHTTPClientForTUI = func(_ context.Context, _ string, target daemonTarget, kind clientOptsKind) (*http.Client, error) {
		if kind == clientOptsNormal {
			gotNormal = target
		} else {
			gotSSE = target
		}
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "http://daemon.internal:7777", conn.endpoint)
	assert.Equal(t, target, gotNormal)
	assert.Equal(t, target, gotSSE)
	assert.Equal(t, "shared", conn.target.Name)
}

func TestBuildRunModelCarriesDaemonMetadata(t *testing.T) {
	conn := daemonConnection{
		target:  daemonTarget{Name: "shared", URL: "https://kata.example.test"},
		catalog: []daemonTarget{{Name: "local", Local: true}, {Name: "shared", URL: "https://kata.example.test"}},
		init:    bootInit{view: viewEmpty, scope: scope{empty: true}},
	}

	m := buildRunModel(Options{}, &Client{}, conn.init, conn)

	assert.Equal(t, "shared", m.activeDaemon.Name)
	require.Len(t, m.daemonTargets, 2)
}

func TestDaemonConnectionUsesSSEHeaderTimeout(t *testing.T) {
	opts := optsForKind(clientOptsSSE)

	assert.Equal(t, clientSSEHandshakeTimeout(), opts.ResponseHeaderTimeout)
	assert.Zero(t, opts.Timeout)
}

func clientSSEHandshakeTimeout() time.Duration {
	return 10 * time.Second
}
