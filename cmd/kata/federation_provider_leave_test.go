package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/federationprovider"
)

// The helper is an external executable. It accepts release only for the exact
// saved request, after the daemon has durably marked the connection as leaving.
func TestLeaveProviderProcess(_ *testing.T) {
	if os.Getenv("KATA_TEST_LEAVE_PROVIDER") != "1" {
		return
	}
	r, err := federationprovider.DecodeRequest(os.Stdin)
	if err != nil || r.Operation != "release" {
		os.Exit(2)
	}
	entries, err := config.ReadFederationCredentials()
	if err != nil {
		os.Exit(2)
	}
	for _, c := range entries.Projects {
		if c.Provider != nil && c.Provider.RequestID == r.RequestID && c.LeavePending {
			err := federationprovider.WriteResponse(os.Stdout, r, federationprovider.Response{
				Version: 1, Operation: r.Operation, RequestID: r.RequestID,
				Status: federationprovider.Status(os.Getenv("KATA_TEST_LEAVE_DECISION")),
			})
			if err == nil {
				os.Exit(0)
			}
		}
	}
	os.Exit(2)
}

func TestProviderCleanupStartupReportsUnreadableCredentials(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.Home, "credentials.toml"), []byte("[invalid"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workers := newDaemonWorkerGroup()
	var health func() api.FederationConfigHealth
	require.NotPanics(t, func() {
		health = startFederationConfigReconciler(ctx, workers, nil, &config.DaemonConfig{}, env.DB, nil, nil, nil)
	})
	require.NotNil(t, health)
	waitCtx, stopWaiting := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopWaiting()
	require.True(t, workers.Wait(waitCtx))
	assert.Equal(t, "credential_io", health().LastErrorCategory)
	assert.Positive(t, health().Pending)
}

func TestFederationLeaveUsesProviderAndRetainsOfflineCleanup(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	t.Setenv("KATA_TEST_LEAVE_PROVIDER", "1")
	t.Setenv("KATA_TEST_LEAVE_DECISION", "unavailable")
	executable, err := os.Executable()
	require.NoError(t, err)
	var hubCalls atomic.Int32
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hubCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(hub.Close)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = hub.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	project := seedLeaveSpoke(t, env, "spoke-project", hub.URL, 42)
	credentials := config.DefaultFederationCredentialStore()
	c, found, err := credentials.FederationCredential(t.Context(), project.UID)
	require.NoError(t, err)
	require.True(t, found)
	c.ManagedByConfig, c.HubCatalog, c.HubProjectName, c.SpokeProjectName = true, "team-hub", "hub-project", project.Name
	c.Provider = &config.FederationProviderCredential{
		RequestID: uuid.New(), Command: []string{executable, "-test.run=^TestLeaveProviderProcess$"},
		Intent: "collaborate", Status: "ready", SpokeInstanceUID: env.DB.InstanceUID(),
		LocalProjectUID: project.UID, HubProjectUID: project.UID, EnrollmentID: 7,
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), project.UID, c))
	peer := config.FederationCredential{HubURL: "https://other.example", Token: "synthetic-peer-token"}
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), "peer-project", peer))
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Keep this task", Author: "Example User"})
	require.NoError(t, err)
	_, _, err = runCmdCapture(t, env, "federation", "leave", "--project", project.Name, "--delete", "--yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open issues")
	untouched, found, err := credentials.FederationCredential(t.Context(), project.UID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, c.Equal(untouched), "archive refusal must precede provider release")

	_, _, err = runCmdCapture(t, env, "federation", "leave", "--project", project.Name, "--yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cleanup is pending")
	binding, err := env.DB.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.False(t, binding.Enabled, "offline release must stop local transport")
	retained, found, err := credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, retained.Credential.LeavePending)
	assert.Equal(t, c.Provider.RequestID, retained.Credential.Provider.RequestID)
	assert.True(t, c.Token == retained.Credential.Token, "offline cleanup must retain the exact candidate")
	status, err := runCmdOutput(t, env, "federation", "status", "--project", project.Name)
	require.NoError(t, err)
	assert.Contains(t, status, "cleanup pending")
	assert.Contains(t, status, "2030-01-01")
	assert.False(t, strings.Contains(status, c.Token), "status must not expose the credential")
	statusJSON, err := runCmdOutput(t, env, "--json", "federation", "status", "--project", project.Name)
	require.NoError(t, err)
	var wire api.FederationStatusBody
	require.NoError(t, json.Unmarshal([]byte(statusJSON), &wire))
	require.Len(t, wire.Statuses, 1)
	assert.Equal(t, "cleanup_pending", wire.Statuses[0].ProviderStatus)
	require.NotNil(t, wire.Statuses[0].CredentialExpiresAt)
	assert.Equal(t, c.Provider.ExpiresAt, *wire.Statuses[0].CredentialExpiresAt)
	runner := federation.Runner{DB: env.DB, Credentials: credentials}
	require.NoError(t, runner.RunOnce(t.Context()))
	assert.Zero(t, hubCalls.Load(), "a normal sync pass must make no request while cleanup is pending")

	t.Setenv("KATA_TEST_LEAVE_DECISION", "released")
	out, _, err := runCmdCapture(t, env, "federation", "leave", "--project", project.Name, "--yes")
	require.NoError(t, err)
	assert.Contains(t, out, "standalone")
	_, err = env.DB.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	closed, found, err := credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	require.True(t, found, "retain the closed request until its config mapping is removed")
	assert.Equal(t, federationprovider.StatusReleased, closed.Credential.Provider.Status)
	assert.Empty(t, closed.Credential.Token, "confirmed cleanup no longer needs the secret")
	peerAfter, found, err := credentials.FederationCredential(t.Context(), "peer-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, peer.Equal(peerAfter))
	assert.Zero(t, hubCalls.Load(), "provider leave must not try catalog administration")
	status, err = runCmdOutput(t, env, "federation", "status", "--project", project.Name)
	require.NoError(t, err)
	assert.Contains(t, status, "released")

	// Daemon composition must run cleanup even after the last mapping was
	// removed. Exercise the production startup function, not just its adapter.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workers := newDaemonWorkerGroup()
	health := startFederationConfigReconciler(ctx, workers, nil, &config.DaemonConfig{}, env.DB, cancel, nil, nil)
	require.NotNil(t, health)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer waitCancel()
	require.True(t, workers.Wait(waitCtx))
	_, found, err = credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	assert.False(t, found, "mapping removal completes local cleanup")
}
