package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
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
	case <-time.After(300 * time.Millisecond):
		t.Fatal("profile identity probe exceeded configured request timeout")
	}
}

func TestLocalProfileStorageProbeHonorsTimeout(t *testing.T) {
	t.Setenv("KATA_HTTP_TIMEOUT", "50ms")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
