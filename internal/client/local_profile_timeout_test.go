package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestLocalProfileIdentityProbeHonorsTimeout(t *testing.T) {
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer s.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- verifyLocalProfileInstance(ctx, ResolvedDaemon{BaseURL: s.URL, LocalProfile: &LocalProfileIdentity{InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01"}})
	}()
	select {
	case <-result:
	// Under DefaultHTTPTimeout, so only the 50ms setting returns the probe in time.
	case <-time.After(2 * time.Second):
		t.Fatal("profile identity probe exceeded configured request timeout")
	}
}

func TestNamedReadOnlyProfileDiscoveryDefersIdentityProbe(t *testing.T) {
	profile, _ := localProfileFixture(t)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())

	var identityCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
		case "/api/v1/instance":
			identityCalls.Add(1)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"),
	})
	require.NoError(t, err)
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")

	resolved, _, err := DiscoverSelected(t.Context(), DiscoverRequest{Name: "work", ReadOnly: true})
	require.NoError(t, err, "read-only named discovery defers profile identity checks to the caller's bounded client")
	require.Equal(t, profile.InstanceUID, resolved.LocalProfile.InstanceUID)
	require.Equal(t, server.URL, resolved.BaseURL)
	require.Zero(t, identityCalls.Load(), "discovery must not make an identity request with KATA_HTTP_TIMEOUT")
}

func TestLocalProfileStorageProbeHonorsTimeout(t *testing.T) {
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := make(chan struct{})
	defer close(release)
	defer func() { _ = listener.Close() }()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer func() { _ = connection.Close() }()
			<-release
		}
	}()
	profile := config.LocalProfileConfig{DSN: fmt.Sprintf("postgres://profile_test:synthetic-secret@%s/example_db?sslmode=disable", listener.Addr()), Config: &config.DaemonConfig{}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err = InspectLocalProfileStorage(ctx, profile)
	require.ErrorIs(t, err, ErrProfileStorageUnavailable)
	require.Contains(t, err.Error(), "timeout")
	require.NotContains(t, err.Error(), "synthetic-secret")
	require.Less(t, time.Since(started), time.Second, "a stalled database must honor the configured inspection budget")
}

func TestLocalProfileIdentityProbeDistinguishesAuthenticationFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "response-body-secret", status)
			}))
			t.Cleanup(server.Close)
			err := verifyLocalProfileInstance(t.Context(), ResolvedDaemon{BaseURL: server.URL, LocalProfile: &LocalProfileIdentity{InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01"}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "response-body-secret")
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				require.Contains(t, err.Error(), "authentication failed")
			} else {
				require.NotContains(t, err.Error(), "authentication failed")
				require.Contains(t, err.Error(), "identity unavailable")
			}
		})
	}
}

func TestLocalProfileUnreachableInstanceKeepsEvidence(t *testing.T) {
	profile, root := localProfileFixture(t)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	entered := make(chan struct{}, 1)
	waitEntered := func(t *testing.T) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("profile identity request did not start")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, currentVersionForEnsure(), os.Getpid())
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"discover", func() error { _, _, err := DiscoverLocalProfileSelection(t.Context(), selection); return err }},
		{"ensure", func() error { _, err := EnsureLocalProfileSelection(t.Context(), selection); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			waitEntered(t)
			var evidence *LocalDaemonUnreachableError
			require.ErrorAs(t, err, &evidence)
			require.Equal(t, "work", evidence.Profile)
			require.Equal(t, ns.DataDir, evidence.DataDir)
			require.Equal(t, os.Getpid(), evidence.PID)
			require.Contains(t, err.Error(), fmt.Sprintf("%q", filepath.Join(ns.DataDir, "daemon.log")))
			require.Contains(t, err.Error(), "recover")
			require.Contains(t, err.Error(), "work")
			require.NotContains(t, err.Error(), "kata daemon restart")
		})
	}
	t.Run("supervised", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
		t.Cleanup(func() { require.NoError(t, os.Remove(filepath.Join(profile.Home, "config.toml"))) })
		selected, err := InspectSelection(t.Context(), root, "")
		require.NoError(t, err)
		_, _, err = DiscoverLocalProfileSelection(t.Context(), selected)
		waitEntered(t)
		var evidence *LocalDaemonUnreachableError
		require.ErrorAs(t, err, &evidence)
		require.Equal(t, os.Getpid(), evidence.PID)
		require.Contains(t, err.Error(), fmt.Sprintf("%q", filepath.Join(ns.DataDir, "daemon.log")))
		require.Contains(t, err.Error(), "service manager")
		require.NotContains(t, err.Error(), "daemon recover")
	})

	t.Run("caller cancellation", func(t *testing.T) {
		t.Setenv("KATA_HTTP_TIMEOUT", "2s")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			select {
			case <-entered:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, _, err := DiscoverLocalProfileSelection(ctx, selection)
		require.ErrorIs(t, err, context.Canceled)
		var evidence *LocalDaemonUnreachableError
		require.ErrorAs(t, err, &evidence)
		require.Equal(t, os.Getpid(), evidence.PID)
		require.Equal(t, ns.DataDir, evidence.DataDir)
		require.Contains(t, err.Error(), fmt.Sprintf("%q", filepath.Join(ns.DataDir, "daemon.log")))
	})

}
