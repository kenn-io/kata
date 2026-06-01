package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestNewClaimHubClientHonorsTrustedPrivateNetwork(t *testing.T) {
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	client, err := newClaimHubClient(context.Background(), "http://100.64.0.5:7787", "enrollment-token")

	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, "http://100.64.0.5:7787", client.baseURL)
}

func TestClaimHubClientUsesUnixRuntimeForKataInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets unsupported on windows")
	}
	tmp, err := os.MkdirTemp("/tmp", "kata-claim-unix-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	t.Setenv("TMPDIR", tmp)
	ns, err := NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	sock := filepath.Join(ns.SocketDir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var gotPath string
	var gotAuth string
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/ping":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":      true,
					"service": "kata",
					"version": "test",
					"pid":     os.Getpid(),
				})
			case "/api/v1/projects/42/issues/ABC/lease/actions/acquire":
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				_ = json.NewEncoder(w).Encode(api.ClaimActionResponseBody{Granted: true})
			default:
				http.NotFound(w, r)
			}
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   "unix",
		Address:   sock,
		Metadata:  map[string]string{"db_path": filepath.Join(tmp, "kata.db")},
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	client, err := newClaimHubClient(context.Background(), "http://kata.invalid", "enrollment-token")
	require.NoError(t, err)
	_, err = client.AcquireClaim(context.Background(), 42, "ABC", api.ClaimActionBody{})

	require.NoError(t, err)
	assert.Equal(t, "/api/v1/projects/42/issues/ABC/lease/actions/acquire", gotPath)
	assert.Equal(t, "Bearer enrollment-token", gotAuth)
}
