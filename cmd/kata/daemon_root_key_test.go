package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

// A hub restored onto a new host without federation-root.key still starts.
// Only signed writes to its pinned project fail, with the recovery command.
func TestDaemonStartsWithPinnedProjectAndMissingRootKey(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
	orig := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
	t.Cleanup(func() { newTelemetryReporter = orig })

	ctx := context.Background()
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	hub, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: hub.ID, Role: db.FederationRoleHub, HubProjectID: hub.ID, HubProjectUID: hub.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: hub.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	standalone, err := store.CreateProject(ctx, "standalone-project")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	daemonCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runDaemonWithListen(daemonCtx, "127.0.0.1:0", false, false) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon did not stop cleanly: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop after context cancellation")
		}
	})
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	var record daemonRuntimeRecordJSON
	require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: daemon startup publishes readiness only through its runtime record file
		select {
		case err := <-done:
			t.Fatalf("daemon exited during startup: %v", err)
		default:
		}
		body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned runtime path
		return readErr == nil && json.Unmarshal(body, &record) == nil
	}, 5*time.Second, 20*time.Millisecond)
	_, err = os.Stat(filepath.Join(ns.DataDir, "federation-root.key"))
	require.ErrorIs(t, err, os.ErrNotExist, "a pinned instance never generates a replacement key at startup")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + record.Address + "/api/v1/health") //nolint:noctx // bounded test client
	require.NoError(t, err)
	var health struct {
		RootSigning struct {
			KeyLoaded bool     `json:"key_loaded"`
			Projects  []string `json:"projects"`
		} `json:"root_signing"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))
	require.NoError(t, resp.Body.Close())
	require.False(t, health.RootSigning.KeyLoaded)
	require.Equal(t, []string{"hub-project"}, health.RootSigning.Projects)

	create := func(projectID int64) (int, string) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("http://%s/api/v1/projects/%d/issues", record.Address, projectID),
			bytes.NewBufferString(`{"actor":"local-owner","title":"Restored host task","force_new":true}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		var body bytes.Buffer
		_, err = body.ReadFrom(response.Body)
		require.NoError(t, err)
		return response.StatusCode, body.String()
	}
	status, body := create(hub.ID)
	require.Equal(t, http.StatusConflict, status, body)
	require.Contains(t, body, "kata federation root-key repin --project hub-project")
	status, body = create(standalone.ID)
	require.Equal(t, http.StatusOK, status, body)

	t.Chdir(t.TempDir())
	out, err := runCmdOutput(t, nil, "daemon", "status")
	require.NoError(t, err)
	require.Contains(t, out, "root signing key "+filepath.Join(ns.DataDir, "federation-root.key")+" is unavailable (key file is missing)")
	require.Contains(t, out, "signed writes to hub-project fail")
	require.Contains(t, out, "kata federation root-key repin --project <name>")
}
