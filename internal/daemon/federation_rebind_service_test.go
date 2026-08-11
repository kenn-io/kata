package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

const (
	rebindSourceURL = "http://192.0.2.10:7777"
	rebindTargetURL = "https://hub.example/kata"
)

type rebindFailingStore struct {
	db.Storage
	err error
}

type rebindBlockingClaimCommitStore struct {
	db.Storage
	commitStarted chan struct{}
	commitRelease chan struct{}
}

func (s rebindBlockingClaimCommitStore) ApplyClaimStatus(
	ctx context.Context,
	projectID int64,
	issueUID string,
	status db.ClaimStatus,
) error {
	close(s.commitStarted)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.commitRelease:
	}
	return s.Storage.ApplyClaimStatus(ctx, projectID, issueUID, status)
}

func (s rebindFailingStore) RebindFederationBinding(
	context.Context, db.RebindFederationBindingParams,
) (db.FederationBinding, error) {
	return db.FederationBinding{}, s.err
}

func TestRebindFederationReplicaRejectsTargetBeforeCredentialRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog config.CatalogDaemonConfig
	}{
		{name: "missing name", catalog: config.CatalogDaemonConfig{URL: rebindTargetURL}},
		{name: "local entry", catalog: config.CatalogDaemonConfig{Name: "primary-hub", Local: true}},
		{name: "plaintext", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindSourceURL}},
		{name: "user info", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: "https://user@hub.example"}},
		{name: "query", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: "https://hub.example?x=1"}},
		{name: "empty query", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: "https://hub.example?"}},
		{name: "fragment", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: "https://hub.example#section"}},
		{name: "empty fragment", catalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: "https://hub.example#"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, project, _ := prepareFederationRebind(t, rebindSourceURL)
			credentials := newReplicaCredentialStore()
			fetchCalls := 0

			_, err := daemon.RebindFederationReplica(
				context.Background(), store, credentials,
				daemon.RebindFederationReplicaParams{
					ProjectID: project.ID, HubCatalog: tc.catalog,
					FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
						fetchCalls++
						return api.ProjectFederationBody{}, nil
					},
				},
			)

			require.ErrorIs(t, err, daemon.ErrFederationReplicaInvalidInput)
			assert.Zero(t, credentials.readCalls)
			assert.Zero(t, fetchCalls)
		})
	}
}

func TestRebindFederationReplicaWaitsForOldEndpointSync(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"next_after_id":11}`))
	}))
	t.Cleanup(oldHub.Close)

	store, project, binding := prepareFederationRebind(t, oldHub.URL)
	credentials := newReplicaCredentialStore()
	credential := rebindCredential(oldHub.URL, false, false)
	credentials.put(project.UID, credential)
	syncDone := make(chan error, 1)
	go func() {
		syncDone <- federation.SyncFederationOnce(context.Background(), store, binding, credential)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("old endpoint sync did not reach the hub")
	}

	rebindDone := make(chan error, 1)
	go func() {
		_, err := daemon.RebindFederationReplica(
			context.Background(), store, credentials,
			daemon.RebindFederationReplicaParams{
				ProjectID:  project.ID,
				HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
				FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
					return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
				},
			},
		)
		rebindDone <- err
	}()

	select {
	case err := <-rebindDone:
		t.Fatalf("rebind completed before the old endpoint sync drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	currentBinding, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, oldHub.URL, currentBinding.HubURL)

	close(releaseRequest)
	require.NoError(t, <-syncDone)
	require.NoError(t, <-rebindDone)
	currentBinding, err = store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, rebindTargetURL, currentBinding.HubURL)
}

func TestRebindFederationReplicaWaitsForImmediateClaimForwardCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		suffix string
		body   []byte
	}{
		{name: "acquire", method: http.MethodPost, suffix: "lease/actions/acquire", body: []byte(`{"holder":"spoke-cli","client_kind":"cli","claim_kind":"hard"}`)},
		{name: "renew", method: http.MethodPost, suffix: "lease/actions/renew", body: []byte(`{"holder":"spoke-cli","client_kind":"cli","ttl_seconds":300}`)},
		{name: "release", method: http.MethodPost, suffix: "lease/actions/release", body: []byte(`{"holder":"spoke-cli","client_kind":"cli","reason":"done"}`)},
		{name: "status", method: http.MethodGet, suffix: "lease"},
		{name: "show refresh", method: http.MethodGet},
		{name: "claim gate refresh", method: http.MethodPatch, body: []byte(`{"actor":"sync-agent","title":"updated title"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			commitStarted := make(chan struct{})
			commitRelease := make(chan struct{})
			var releaseOnce sync.Once
			releaseCommit := func() { releaseOnce.Do(func() { close(commitRelease) }) }
			t.Cleanup(releaseCommit)

			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaHubProjectUID)
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID,
				Title:     "claim target",
				Author:    "tester",
			})
			require.NoError(t, err)

			now := time.Now().UTC()
			claim := &api.IssueClaimOut{
				ClaimUID:          replicaLocalProjectUID,
				ProjectID:         42,
				IssueUID:          issue.UID,
				Holder:            "sync-agent",
				HolderInstanceUID: store.InstanceUID(),
				ClientKind:        "cli",
				ClaimKind:         "hard",
				AcquiredAt:        now,
				Revision:          1,
				UpdatedAt:         now,
			}
			oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
						Held: true,
						Holder: api.ClaimPrincipalOut{
							HolderInstanceUID: store.InstanceUID(), Holder: "sync-agent", ClientKind: "cli",
						},
						Claim: claim, HubNow: now,
					}))
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{
					Granted: true,
					Holder: api.ClaimPrincipalOut{
						HolderInstanceUID: store.InstanceUID(), Holder: "sync-agent", ClientKind: "cli",
					},
					Claim: claim,
				}))
			}))
			t.Cleanup(oldHub.Close)

			binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
				ProjectID: project.ID, Role: db.FederationRoleSpoke,
				HubURL: oldHub.URL, HubProjectID: 42, HubProjectUID: project.UID,
				ReplayHorizonEventID: 1, PushEnabled: true, Actor: "sync-agent",
				AllowInsecure: true, Enabled: true,
			})
			require.NoError(t, err)
			credentials := newReplicaCredentialStore()
			credential := rebindCredential(oldHub.URL, false, false)
			credential.Capabilities = "claim"
			credentials.put(project.UID, credential)

			blockingStore := rebindBlockingClaimCommitStore{
				Storage: store, commitStarted: commitStarted, commitRelease: commitRelease,
			}
			server := startTestServer(t, daemon.ServerConfig{
				DB: blockingStore, StartedAt: now, FederationCredentials: credentials,
				FederationCatalog: []config.CatalogDaemonConfig{{
					Name: "primary-hub", URL: rebindTargetURL,
				}},
				FederationRebindFetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
					return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
				},
			})
			claimPath := issuePathRef(project.ID, issue.ShortID, tc.suffix)
			claimDone := make(chan error, 1)
			go func() {
				req, requestErr := http.NewRequestWithContext(
					ctx, tc.method, server.URL+claimPath, bytes.NewReader(tc.body),
				)
				if requestErr != nil {
					claimDone <- requestErr
					return
				}
				if len(tc.body) > 0 {
					req.Header.Set("Content-Type", "application/json")
				}
				resp, requestErr := http.DefaultClient.Do(req)
				if requestErr != nil {
					claimDone <- requestErr
					return
				}
				_, copyErr := io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if copyErr != nil {
					claimDone <- copyErr
					return
				}
				if closeErr != nil {
					claimDone <- closeErr
					return
				}
				if resp.StatusCode != http.StatusOK {
					claimDone <- fmt.Errorf("claim request returned HTTP %d", resp.StatusCode)
					return
				}
				claimDone <- nil
			}()

			select {
			case <-commitStarted:
			case <-time.After(time.Second):
				t.Fatal("forwarded claim response did not reach its local commit")
			}

			rebindDone := make(chan error, 1)
			go func() {
				req, requestErr := http.NewRequestWithContext(
					ctx,
					http.MethodPost,
					fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/rebind", server.URL, project.ID),
					bytes.NewBufferString(`{"hub_catalog":"primary-hub"}`),
				)
				if requestErr != nil {
					rebindDone <- requestErr
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, requestErr := http.DefaultClient.Do(req)
				if requestErr != nil {
					rebindDone <- requestErr
					return
				}
				_, copyErr := io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if copyErr != nil {
					rebindDone <- copyErr
					return
				}
				if closeErr != nil {
					rebindDone <- closeErr
					return
				}
				if resp.StatusCode != http.StatusOK {
					rebindDone <- fmt.Errorf("rebind request returned HTTP %d", resp.StatusCode)
					return
				}
				rebindDone <- nil
			}()

			select {
			case rebindErr := <-rebindDone:
				releaseCommit()
				t.Fatalf("rebind completed before the forwarded claim committed: %v", rebindErr)
			case <-time.After(100 * time.Millisecond):
			}

			releaseCommit()
			require.NoError(t, <-claimDone)
			require.NoError(t, <-rebindDone)
			converged, err := store.FederationBindingByProject(ctx, binding.ProjectID)
			require.NoError(t, err)
			assert.Equal(t, rebindTargetURL, converged.HubURL)
		})
	}
}

func TestRebindFederationReplicaDistinguishesReverseProxyPaths(t *testing.T) {
	const sourceURL = "https://hub.example/old-prefix"
	const targetURL = "https://hub.example/new-prefix"
	store, project, _ := prepareFederationRebind(t, sourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(sourceURL, false, false))

	result, err := daemon.RebindFederationReplica(
		context.Background(), store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: targetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)

	require.NoError(t, err)
	assert.Equal(t, daemon.FederationRebindStateRebound, result.State)
	assert.Equal(t, targetURL, result.Binding.HubURL)
}

func TestRebindFederationReplicaConvergesCanonicalTargetFromRawDefaultPort(t *testing.T) {
	const equivalentTargetURL = "https://hub.example:443/kata"
	ctx := context.Background()
	store, project, binding := prepareFederationRebind(t, rebindTargetURL)
	binding.HubURL = equivalentTargetURL
	binding.AllowInsecure = false
	_, err := store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(equivalentTargetURL, true, false))

	result, err := daemon.RebindFederationReplica(
		ctx, store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)

	require.NoError(t, err)
	assert.Equal(t, daemon.FederationRebindStateUnchanged, result.State)
	converged, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, rebindTargetURL, converged.HubURL)
}

func TestRebindFederationReplicaConvergesEveryDurableState(t *testing.T) {
	for _, tc := range []struct {
		name             string
		credentialTarget bool
		bindingTarget    bool
		managed          bool
		wantState        daemon.FederationRebindState
	}{
		{name: "fully old", wantState: daemon.FederationRebindStateRebound},
		{name: "credential first", credentialTarget: true, managed: true, wantState: daemon.FederationRebindStateResumed},
		{name: "binding first", bindingTarget: true, managed: true, wantState: daemon.FederationRebindStateResumed},
		{name: "fully target", credentialTarget: true, bindingTarget: true, managed: true, wantState: daemon.FederationRebindStateUnchanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentialURL, bindingURL := rebindSourceURL, rebindSourceURL
			if tc.credentialTarget {
				credentialURL = rebindTargetURL
			}
			if tc.bindingTarget {
				bindingURL = rebindTargetURL
			}
			store, project, originalBinding := prepareFederationRebind(t, bindingURL)
			credentials := newReplicaCredentialStore()
			credential := rebindCredential(credentialURL, tc.credentialTarget, tc.managed)
			credentials.put(project.UID, credential)
			fetchCalls := 0

			result, err := daemon.RebindFederationReplica(
				context.Background(), store, credentials,
				daemon.RebindFederationReplicaParams{
					ProjectID: project.ID,
					HubCatalog: config.CatalogDaemonConfig{ //nolint:gosec // contains a non-secret test sentinel
						Name: "primary-hub", URL: rebindTargetURL,
						Token:    "catalog-admin-secret",
						TokenEnv: "CATALOG_TOKEN_ENV",
					},
					FetchMetadata: func(_ context.Context, hubURL, token string, projectID int64) (api.ProjectFederationBody, error) {
						fetchCalls++
						assert.Equal(t, rebindTargetURL, hubURL)
						assert.Equal(t, "enrollment-secret", token)
						assert.Equal(t, int64(42), projectID)
						return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
					},
				},
			)

			require.NoError(t, err)
			assert.Equal(t, tc.wantState, result.State)
			wantPreviousURL := rebindSourceURL
			if tc.credentialTarget && tc.bindingTarget {
				wantPreviousURL = rebindTargetURL
			}
			assert.Equal(t, wantPreviousURL, result.PreviousHubURL)
			assert.Equal(t, 1, fetchCalls)
			storedCredential, found, readErr := credentials.FederationCredential(
				context.Background(), project.UID,
			)
			require.NoError(t, readErr)
			require.True(t, found)
			wantCredential := credential
			wantCredential.HubURL = rebindTargetURL
			wantCredential.AllowInsecure = false
			assert.Equal(t, wantCredential, storedCredential)
			storedBinding, readErr := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, readErr)
			assert.Equal(t, rebindTargetURL, storedBinding.HubURL)
			assert.False(t, storedBinding.AllowInsecure)
			assert.Equal(t, originalBinding.ReplayHorizonEventID, storedBinding.ReplayHorizonEventID)
			assert.Equal(t, originalBinding.PullCursorEventID, storedBinding.PullCursorEventID)
			assert.Equal(t, originalBinding.PushCursorEventID, storedBinding.PushCursorEventID)
			assert.Equal(t, originalBinding.Actor, storedBinding.Actor)
			assert.Equal(t, originalBinding.PushEnabled, storedBinding.PushEnabled)
		})
	}
}

func TestRebindFederationReplicaConvergesLegacyCredentialOnlyInsecureState(t *testing.T) {
	ctx := context.Background()
	store, project, binding := prepareFederationRebind(t, rebindSourceURL)
	binding.AllowInsecure = false
	_, err := store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))

	result, err := daemon.RebindFederationReplica(
		ctx, store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)

	require.NoError(t, err)
	assert.Equal(t, daemon.FederationRebindStateRebound, result.State)
	assert.Equal(t, rebindTargetURL, result.Binding.HubURL)
	assert.False(t, result.Binding.AllowInsecure)
	credential, found, err := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rebindTargetURL, credential.HubURL)
	assert.False(t, credential.AllowInsecure)
}

func TestRebindFederationReplicaRemoteValidationFailureLeavesLocalStateUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata api.ProjectFederationBody
		fetchErr error
	}{
		{name: "project ID mismatch", metadata: api.ProjectFederationBody{ProjectID: 43, ProjectUID: replicaHubProjectUID}},
		{name: "project UID mismatch", metadata: api.ProjectFederationBody{ProjectID: 42, ProjectUID: replicaLocalProjectUID}},
		{name: "enrollment rejected", fetchErr: daemon.ErrFederationReplicaCredentialConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, project, originalBinding := prepareFederationRebind(t, rebindSourceURL)
			credentials := newReplicaCredentialStore()
			originalCredential := rebindCredential(rebindSourceURL, false, false)
			credentials.put(project.UID, originalCredential)

			_, err := daemon.RebindFederationReplica(
				context.Background(), store, credentials,
				daemon.RebindFederationReplicaParams{
					ProjectID:  project.ID,
					HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
					FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
						return tc.metadata, tc.fetchErr
					},
				},
			)

			require.Error(t, err)
			storedCredential, found, readErr := credentials.FederationCredential(context.Background(), project.UID)
			require.NoError(t, readErr)
			require.True(t, found)
			assert.Equal(t, originalCredential, storedCredential)
			storedBinding, readErr := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, readErr)
			assert.Equal(t, originalBinding, storedBinding)
		})
	}
}

func TestRebindFederationReplicaResumesAfterBindingWriteFailure(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	params := daemon.RebindFederationReplicaParams{
		ProjectID:  project.ID,
		HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
		FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
			return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
		},
	}

	_, err := daemon.RebindFederationReplica(
		context.Background(), rebindFailingStore{Storage: store, err: errors.New("injected binding failure")},
		credentials, params,
	)
	require.ErrorContains(t, err, "injected binding failure")
	credentialAfterFailure, found, readErr := credentials.FederationCredential(context.Background(), project.UID)
	require.NoError(t, readErr)
	require.True(t, found)
	assert.Equal(t, rebindTargetURL, credentialAfterFailure.HubURL)
	bindingAfterFailure, readErr := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, readErr)
	assert.Equal(t, rebindSourceURL, bindingAfterFailure.HubURL)

	result, err := daemon.RebindFederationReplica(context.Background(), store, credentials, params)
	require.NoError(t, err)
	assert.Equal(t, daemon.FederationRebindStateResumed, result.State)
	converged, readErr := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, readErr)
	assert.Equal(t, rebindTargetURL, converged.HubURL)
}

func TestRebindFederationReplicaCredentialFailureLeavesBindingUnchanged(t *testing.T) {
	store, project, originalBinding := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	credentials.replaceErr = errors.New("injected credential failure")

	_, err := daemon.RebindFederationReplica(
		context.Background(), store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialIO)
	storedBinding, readErr := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, readErr)
	assert.Equal(t, originalBinding, storedBinding)
}

func TestRebindFederationReplicaManagedCatalogMustMatch(t *testing.T) {
	store, project, originalBinding := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credential := rebindCredential(rebindSourceURL, false, true)
	credentials.put(project.UID, credential)
	fetchCalls := 0

	_, err := daemon.RebindFederationReplica(
		context.Background(), store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "another-hub", URL: rebindTargetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				fetchCalls++
				return api.ProjectFederationBody{}, nil
			},
		},
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	assert.Zero(t, fetchCalls)
	storedCredential, found, readErr := credentials.FederationCredential(context.Background(), project.UID)
	require.NoError(t, readErr)
	require.True(t, found)
	assert.Equal(t, credential, storedCredential)
	storedBinding, readErr := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, readErr)
	assert.Equal(t, originalBinding, storedBinding)
}

func TestRebindFederationReplicaPreservesCursorAdvancementDuringPreflight(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))

	result, err := daemon.RebindFederationReplica(
		context.Background(), store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
			FetchMetadata: func(ctx context.Context, _ string, _ string, _ int64) (api.ProjectFederationBody, error) {
				require.NoError(t, store.AdvanceFederationPullCursor(ctx, project.ID, 25))
				require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, 26))
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)

	require.NoError(t, err)
	assert.Equal(t, int64(25), result.Binding.PullCursorEventID)
	assert.Equal(t, int64(26), result.Binding.PushCursorEventID)
}

func TestRebindFederationReplicaDrainsBeforePreparedLeave(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store, project, originalBinding := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credential := rebindCredential(rebindSourceURL, false, true)
	credentials.put(project.UID, credential)
	fetchStarted := make(chan struct{})
	fetchRelease := make(chan struct{})
	rebindErr := make(chan error, 1)
	go func() {
		_, err := daemon.RebindFederationReplica(
			ctx, store, credentials,
			daemon.RebindFederationReplicaParams{
				ProjectID:  project.ID,
				HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
				FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
					close(fetchStarted)
					select {
					case <-fetchRelease:
						return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
					case <-ctx.Done():
						return api.ProjectFederationBody{}, ctx.Err()
					}
				},
			},
		)
		rebindErr <- err
	}()
	select {
	case <-fetchStarted:
	case <-ctx.Done():
		require.FailNow(t, "wait for rebind preflight", "error: %v", ctx.Err())
	}

	prepareDone := make(chan error, 1)
	var prepared atomic.Bool
	go func() {
		_, err := daemon.PrepareFederationReplicaLeave(ctx, store, credentials, project.ID)
		prepared.Store(true)
		prepareDone <- err
	}()
	require.Eventually(t, func() bool {
		current, found, err := credentials.FederationCredential(ctx, project.UID)
		return err == nil && found && current.LeavePending
	}, time.Second, time.Millisecond)
	assert.False(t, prepared.Load(), "leave preparation returned before rebind drained")
	close(fetchRelease)

	select {
	case err := <-rebindErr:
		require.ErrorIs(t, err, daemon.ErrFederationReplicaLeavePending)
	case <-ctx.Done():
		require.FailNow(t, "wait for rebind to observe leave", "error: %v", ctx.Err())
	}
	select {
	case err := <-prepareDone:
		require.NoError(t, err)
	case <-ctx.Done():
		require.FailNow(t, "wait for prepared leave", "error: %v", ctx.Err())
	}
	storedBinding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, originalBinding, storedBinding)
}

func prepareFederationRebind(
	t *testing.T,
	bindingURL string,
) (db.Storage, db.Project, db.FederationBinding) {
	t.Helper()
	store := openReplicaServiceStore(t)
	ctx := context.Background()
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	bindingTarget := bindingURL == rebindTargetURL
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: bindingURL, HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 10, PullCursorEventID: 11,
		PushEnabled: true, PushCursorEventID: 12,
		Actor: "sync-agent", AllowInsecure: !bindingTarget, Enabled: true,
	})
	require.NoError(t, err)
	return store, project, binding
}

func rebindCredential(hubURL string, target bool, managed bool) config.FederationCredential {
	credential := config.FederationCredential{
		HubURL: hubURL, HubProjectID: 42, Token: "enrollment-secret",
		Capabilities: "pull,push", Actor: "sync-agent", AllowInsecure: !target,
	}
	if managed {
		credential.ManagedByConfig = true
		credential.HubCatalog = "primary-hub"
		credential.HubProjectName = "hub-project"
		credential.RequestedActor = "requested-user"
		credential.SpokeProjectName = "spoke-project"
	}
	return credential
}
