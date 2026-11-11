package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestReadOnlyDiscoveryDoesNotCreateRuntimeDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, found, err := DiscoverResolvedReadOnly(t.Context(), path)
	require.NoError(t, err)
	require.False(t, found)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "discovery must not materialize runtime state")
}

func TestNormalDiscoveryPreservesRuntimeDirectoryPreparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, found, err := Discover(t.Context(), path)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, safefileio.ValidatePrivateDir(path))
}

func TestDiscoveryRepairsExistingRuntimeDirectoryPrivacyWithoutChangingReadOnlyScan(t *testing.T) {
	dir := t.TempDir()
	assertUnchanged := makeRuntimeDirectoryInsecureForTest(t, dir)

	_, err := readRuntimeRecords(dir)
	require.Error(t, err, "the read-only reader must reject a runtime directory with unsafe permissions")
	assertUnchanged()

	_, found, err := Discover(t.Context(), dir)
	require.NoError(t, err, "normal discovery must preserve RuntimeStore.List's permission repair")
	require.False(t, found)
	require.NoError(t, safefileio.ValidatePrivateDir(dir), "normal discovery must repair the runtime directory")
}

func TestReadRuntimeRecordsRecognizesKitFieldsAndLargeRecords(t *testing.T) {
	for _, body := range []string{
		`{"PID":123,"Address":"127.0.0.1:7777"}`,
		fmt.Sprintf(`{"pid":123,"address":"127.0.0.1:7777","metadata":{"example":%q}}`, strings.Repeat("x", (1<<20)+1)),
	} {
		dir := t.TempDir()
		require.NoError(t, safefileio.EnsurePrivateDir(dir))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.123.json"), []byte(body), 0600))
		want, err := (kitdaemon.RuntimeStore{Dir: dir}).List()
		require.NoError(t, err)
		require.Len(t, want, 1)
		got, err := readRuntimeRecords(dir)
		require.NoError(t, err)
		require.Equal(t, want, got, "doctor and normal discovery must recognize the same runtime record")
	}
}

func TestDiscoveryRedirectPolicyIsSpecificToDoctor(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			http.Redirect(w, r, "/ping", http.StatusFound)
			return
		}
		redirected.Add(1)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	_, err := (kitdaemon.RuntimeStore{Dir: dir}).Write(kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{
		Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"),
	}))
	require.NoError(t, err)
	_, found, err := Discover(t.Context(), dir)
	require.NoError(t, err, "normal discovery still follows same-origin redirects")
	require.True(t, found)
	require.EqualValues(t, 1, redirected.Load())
	_, found, err = DiscoverResolvedReadOnly(t.Context(), dir)
	require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	require.False(t, found)
	require.EqualValues(t, 1, redirected.Load(), "doctor discovery must not follow the redirect")
}

func TestReadRuntimeRecordsFindsKitRuntimeStoreRecord(t *testing.T) {
	dir := t.TempDir()
	store := kitdaemon.RuntimeStore{Dir: dir}
	written, err := store.Write(kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{
		Network: "tcp", Address: "127.0.0.1:7777",
	}))
	require.NoError(t, err)

	records, err := readRuntimeRecords(dir)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, os.Getpid(), records[0].PID)
	require.Equal(t, "127.0.0.1:7777", records[0].Address)
	require.Equal(t, written, records[0].SourcePath)
}

// The issue contract is evidence capture and scoped recovery for a live process
// whose listener cannot answer ping; this fixture does not infer the hang's cause.
func TestLiveUnreachableRecoveryAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(server.Close)
	for _, tc := range []struct{ name, origin, want string }{
		{"valid origin", server.URL, server.URL},
		{"malformed origin", "http://user@host/path", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
			require.True(t, ok)
			_, err := (kitdaemon.RuntimeStore{Dir: dir}).Write(kitdaemon.RuntimeRecord{ProcessIdentityV2: identity, Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"), Metadata: map[string]string{"web_origin": tc.origin}})
			require.NoError(t, err)
			_, found, err := DiscoverResolvedReadOnly(t.Context(), dir)
			require.False(t, found)
			require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
			var observed *LocalDaemonUnreachableError
			require.ErrorAs(t, err, &observed)
			require.Contains(t, err.Error(), fmt.Sprintf("daemon pid %d", os.Getpid()))
			require.Contains(t, err.Error(), server.Listener.Addr().String())
			require.Equal(t, dir, observed.DataDir)
			require.Equal(t, tc.want, observed.WebOrigin)
			require.True(t, observed.VerifiedProcess)
		})
	}
	t.Run("unverified", func(t *testing.T) {
		record := kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: "future-v1:12345", Network: "tcp", Address: "127.0.0.1:1"}
		reader := func(string) ([]kitdaemon.RuntimeRecord, error) { return []kitdaemon.RuntimeRecord{record}, nil }
		probe := func(context.Context, string) (string, PingInfo, error) {
			return "", PingInfo{}, errors.New("probe failed")
		}
		var observed error
		for _, err := range liveDaemonsUsing(t.Context(), t.TempDir(), reader, probe) {
			observed = err
			break
		}
		require.ErrorIs(t, observed, ErrLocalDaemonUnreachable)
		var evidence *LocalDaemonUnreachableError
		require.ErrorAs(t, observed, &evidence)
		require.False(t, evidence.VerifiedProcess)
	})
}
