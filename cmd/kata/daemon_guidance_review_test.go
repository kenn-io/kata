package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestHealthUnreadableProjectsRetainsObservedRuntimeEvidence(t *testing.T) {
	home, _, uid, _ := stoppedProfileCLI(t)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "ALTER TABLE projects RENAME TO unavailable_projects")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	address := strings.TrimPrefix(server.URL, "http://")
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: address, Metadata: map[string]string{"web_origin": server.URL}})
	require.NoError(t, err)
	_, _, err = executeRootCapture(t, t.Context(), "--daemon", "work", "health")
	var cliErr *cliError
	require.ErrorAs(t, err, &cliErr)
	require.Equal(t, ExitDaemonUnavail, cliErr.ExitCode)
	require.Equal(t, "unreadable_storage", cliErr.Code)
	var result daemonDiagnosis
	require.NoError(t, json.Unmarshal(cliErr.Data, &result))
	require.Equal(t, "unreadable_storage", result.State)
	require.Equal(t, "work", result.Profile)
	require.Equal(t, home, result.Home)
	require.Equal(t, uid, result.ExpectedInstanceUID)
	require.Contains(t, result.Message, "projects")
	require.Equal(t, os.Getpid(), result.PID)
	require.Contains(t, result.Endpoint, address)
	require.Contains(t, cliErr.Message, "projects")
	require.Contains(t, result.NextAction, filepath.Join(ns.DataDir, "daemon.log"))
	require.Contains(t, result.NextAction, server.URL+"/debug/pprof/goroutine?debug=2")
	require.Contains(t, result.NextAction, "context deadline exceeded")
	require.NotContains(t, result.NextAction, "restart")
	require.NotContains(t, result.NextAction, "kill")
	require.NotContains(t, result.NextAction, "taskkill")
	require.NotContains(t, result.NextAction, "recover")
	require.Equal(t, int32(1), calls.Load())
}

func TestDaemonStatusShowsUnreachableActionAndInventory(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:1"})
	require.NoError(t, err)
	for _, mode := range []string{"", "--agent"} {
		args := []string{"daemon", "status"}
		if mode != "" {
			args = append(args, mode)
		}
		out, _, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err)
		require.Contains(t, out, "local_unreachable")
		require.Contains(t, out, "daemon.log")
		if mode == "" {
			require.Contains(t, out, "next:")
			require.Contains(t, out, "kata running at")
		} else {
			require.Contains(t, out, "next_action=")
			require.Contains(t, out, "status=running")
		}
	}
}
