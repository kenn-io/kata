package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRebindFederationReplicaWaitsForClaimAcrossPostgresDaemons(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	claimStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, claimStore.Close()) })
	rebindStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rebindStore.Close()) })

	project, err := claimStore.CreateProjectWithUID(ctx, "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	issue, _, err := claimStore.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)

	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var oldHubRequests atomic.Int64
	now := time.Now().UTC()
	oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if oldHubRequests.Add(1) == 1 {
			close(requestStarted)
		}
		select {
		case <-releaseResponse:
		case <-ctx.Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{
			Granted: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: claimStore.InstanceUID(), Holder: "spoke-cli", ClientKind: "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          replicaLocalProjectUID,
				ProjectID:         42,
				IssueUID:          issue.UID,
				Holder:            "spoke-cli",
				HolderInstanceUID: claimStore.InstanceUID(),
				ClientKind:        "cli",
				ClaimKind:         "hard",
				AcquiredAt:        now,
				Revision:          1,
				UpdatedAt:         now,
			},
		}))
	}))
	t.Cleanup(oldHub.Close)
	var targetHubRequests atomic.Int64
	targetHub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHubRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: claimStore.InstanceUID(), Holder: "spoke-cli", ClientKind: "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          replicaLocalProjectUID,
				ProjectID:         42,
				IssueUID:          issue.UID,
				Holder:            "spoke-cli",
				HolderInstanceUID: claimStore.InstanceUID(),
				ClientKind:        "cli",
				ClaimKind:         "hard",
				AcquiredAt:        now,
				Revision:          1,
				UpdatedAt:         now,
			},
			HubNow: now,
		}))
	}))
	t.Cleanup(targetHub.Close)
	originalDefaultTransport := http.DefaultTransport
	http.DefaultTransport = targetHub.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalDefaultTransport })

	binding, err := claimStore.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: oldHub.URL, HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 1, PushEnabled: true, Actor: "sync-agent",
		AllowInsecure: true, Enabled: true,
	})
	require.NoError(t, err)
	claimCredentials := newReplicaCredentialStore()
	rebindCredentials := newReplicaCredentialStore()
	credential := rebindCredential(oldHub.URL, false, false)
	credential.Capabilities = "claim"
	claimCredentials.put(project.UID, credential)
	rebindCredentials.put(project.UID, credential)

	claimDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: claimStore, StartedAt: now, FederationCredentials: claimCredentials,
	})
	t.Cleanup(func() { require.NoError(t, claimDaemon.Close()) })
	claimServer := httptest.NewServer(claimDaemon.Handler())
	t.Cleanup(claimServer.Close)

	rebindDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: rebindStore, StartedAt: now, FederationCredentials: rebindCredentials,
		FederationCatalog: []config.CatalogDaemonConfig{{
			Name: "primary-hub", URL: targetHub.URL,
		}},
		FederationRebindFetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
			return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
		},
	})
	t.Cleanup(func() { require.NoError(t, rebindDaemon.Close()) })
	rebindServer := httptest.NewServer(rebindDaemon.Handler())
	t.Cleanup(rebindServer.Close)

	claimDone := make(chan error, 1)
	go func() {
		claimDone <- postFederationClaimAcquire(
			ctx,
			claimServer.URL+issuePathRef(project.ID, issue.ShortID, "lease/actions/acquire"),
		)
	}()
	select {
	case <-requestStarted:
	case <-ctx.Done():
		t.Fatalf("forwarded claim did not reach the old hub: %v", ctx.Err())
	}

	rebindDone := make(chan error, 1)
	go func() {
		rebindDone <- postFederationRebind(ctx, rebindServer.URL, project.ID)
	}()
	select {
	case rebindErr := <-rebindDone:
		close(releaseResponse)
		t.Fatalf("cross-replica rebind completed before the old-origin claim committed: %v", rebindErr)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseResponse)
	require.NoError(t, <-claimDone)
	require.NoError(t, <-rebindDone)
	converged, err := claimStore.FederationBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, targetHub.URL, converged.HubURL)
	claimStatus, err := claimStore.ClaimStatus(ctx, project.ID, issue.UID, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "spoke-cli", claimStatus.Holder.Holder)
	staleCredential, found, err := claimCredentials.FederationCredential(ctx, project.UID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, oldHub.URL, staleCredential.HubURL)

	require.NoError(t, getFederationClaimStatus(
		ctx,
		claimServer.URL+issuePathRef(project.ID, issue.ShortID, "lease"),
	))
	assert.Equal(t, int64(1), oldHubRequests.Load(), "post-cutover claim traffic returned to the old endpoint")
	assert.Equal(t, int64(1), targetHubRequests.Load(), "post-cutover claim traffic did not use the rebound endpoint")
}

func postFederationClaimAcquire(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewBufferString(`{"holder":"spoke-cli","client_kind":"cli","claim_kind":"hard"}`),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return requireSuccessfulFederationResponse(req)
}

func postFederationRebind(ctx context.Context, baseURL string, projectID int64) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/rebind", baseURL, projectID),
		bytes.NewBufferString(`{"hub_catalog":"primary-hub"}`),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return requireSuccessfulFederationResponse(req)
}

func getFederationClaimStatus(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return requireSuccessfulFederationResponse(req)
}

func requireSuccessfulFederationResponse(req *http.Request) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request returned HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
