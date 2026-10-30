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
	"testing/synctest"
	"time"
	"uuid"

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

func (s rebindFailingStore) RebindFederationBinding(context.Context, db.RebindFederationBindingParams) (db.FederationBinding, error) {
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
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence window; the held old endpoint sync keeps the rebind in its drain wait
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
			case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence window; releaseCommit holds the forwarded claim, so the rebind must not complete
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
		_, err := daemon.PrepareFederationReplicaLeave(ctx, store, credentials, project.ID, "")
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

// rebindToTarget runs the rebind an operator retries after a failed leave.
func rebindToTarget(t *testing.T, store db.Storage, credentials config.FederationCredentialStore, project db.Project) error {
	t.Helper()
	_, err := daemon.RebindFederationReplica(
		t.Context(), store, credentials,
		daemon.RebindFederationReplicaParams{
			ProjectID:  project.ID,
			HubCatalog: config.CatalogDaemonConfig{Name: "primary-hub", URL: rebindTargetURL},
			FetchMetadata: func(context.Context, string, string, int64) (api.ProjectFederationBody, error) {
				return api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID}, nil
			},
		},
	)
	return err
}

// A leave whose hub revoke failed never touched local state, so releasing its
// preparation must make the replica rebindable without a daemon restart.
func TestAbortedFederationReplicaLeaveAllowsRebind(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
	require.NoError(t, err)
	require.ErrorIs(t, rebindToTarget(t, store, credentials, project), daemon.ErrFederationReplicaLeavePending)

	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, prepared.PreparationID))

	assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name))
	require.NoError(t, rebindToTarget(t, store, credentials, project))
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, rebindTargetURL, binding.HubURL)
}

func TestAbortFederationReplicaLeaveKeepsLeaveThatMustFinish(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		started func(*testing.T, db.Storage, db.Project)
	}{
		// Preparation durably marks a config-managed reservation; only
		// finishing the leave may clear that marker.
		{name: "durable managed marker", managed: true},
		{name: "binding detached", started: func(t *testing.T, store db.Storage, project db.Project) {
			_, err := store.LeaveFederationReplica(t.Context(), project.ID)
			require.NoError(t, err)
		}},
		{name: "project archived", started: func(t *testing.T, store db.Storage, project db.Project) {
			_, _, err := store.RemoveProject(t.Context(), db.RemoveProjectParams{
				ProjectID: project.ID, Actor: "sync-agent", Force: true, SkipFederationRelay: true,
			})
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, project, _ := prepareFederationRebind(t, rebindSourceURL)
			credentials := newReplicaCredentialStore()
			credentials.put(project.UID, rebindCredential(rebindSourceURL, false, tc.managed))
			prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
			require.NoError(t, err)
			if tc.started != nil {
				tc.started(t, store, project)
			}

			err = daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, prepared.PreparationID)

			require.ErrorIs(t, err, daemon.ErrFederationReplicaLeaveStarted)
			assert.True(t, daemon.FederationReplicaMappingSuppressed(store, project.Name))
		})
	}
}

// Abort releases only the preparation it names. Retrying an abort whose
// response was lost, or naming an unknown preparation, must not release another
// client's leave, which may still revoke and tear down.
func TestAbortFederationReplicaLeaveReleasesOnlyItsPreparation(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	first, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
	require.NoError(t, err)
	second, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
	require.NoError(t, err)

	for range 2 {
		require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, first.PreparationID))
	}
	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, "unknown-preparation"))
	assert.True(t, daemon.FederationReplicaMappingSuppressed(store, project.Name),
		"the second preparation must keep the mapping pending")

	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, second.PreparationID))
	assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name))
}

// A client that lost a prepare response retries prepare with the preparation ID
// it chose. Both calls hold the same preparation, so one abort releases it.
func TestAbortFederationReplicaLeaveReleasesRetriedPreparation(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	for range 2 {
		prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "client-preparation")
		require.NoError(t, err)
		require.Equal(t, "client-preparation", prepared.PreparationID)
	}

	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, "client-preparation"))

	assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name))
	require.NoError(t, rebindToTarget(t, store, credentials, project))
}

// Every daemon service in a process shares the leave registry. Two stores whose
// projects have the same database-local ID, preparing under the same
// preparation ID, must not release each other's preparation.
func TestAbortFederationReplicaLeaveStaysWithinItsStore(t *testing.T) {
	first, firstProject, _ := prepareFederationRebind(t, rebindSourceURL)
	second, secondProject, _ := prepareFederationRebind(t, rebindSourceURL)
	require.Equal(t, firstProject.ID, secondProject.ID)
	require.NotEqual(t, first.InstanceUID(), second.InstanceUID())
	firstCredentials := newReplicaCredentialStore()
	firstCredentials.put(firstProject.UID, rebindCredential(rebindSourceURL, false, false))
	secondCredentials := newReplicaCredentialStore()
	secondCredentials.put(secondProject.UID, rebindCredential(rebindSourceURL, false, false))
	_, err := daemon.PrepareFederationReplicaLeave(t.Context(), first, firstCredentials, firstProject.ID, "shared-preparation")
	require.NoError(t, err)
	_, err = daemon.PrepareFederationReplicaLeave(t.Context(), second, secondCredentials, secondProject.ID, "shared-preparation")
	require.NoError(t, err)

	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), second, secondCredentials, secondProject.ID, "shared-preparation"))

	assert.False(t, daemon.FederationReplicaMappingSuppressed(second, secondProject.Name))
	assert.True(t, daemon.FederationReplicaMappingSuppressed(first, firstProject.Name),
		"another store's abort must not release this store's preparation")
	require.ErrorIs(t, rebindToTarget(t, first, firstCredentials, firstProject), daemon.ErrFederationReplicaLeavePending)
	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), first, firstCredentials, firstProject.ID, "shared-preparation"))
	assert.False(t, daemon.FederationReplicaMappingSuppressed(first, firstProject.Name))
}

// The leave registry keys a manually enrolled spoke by its project name, and a
// project can be renamed while its leave is prepared. Abort must still release
// the preparation, and only through the project that prepared it.
func TestAbortFederationReplicaLeaveAfterProjectRename(t *testing.T) {
	store, project, _ := prepareFederationRebind(t, rebindSourceURL)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, rebindCredential(rebindSourceURL, false, false))
	other, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
	require.NoError(t, err)
	_, err = store.RenameProject(t.Context(), project.ID, "renamed-project")
	require.NoError(t, err)

	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, other.ID, prepared.PreparationID))
	assert.True(t, daemon.FederationReplicaMappingSuppressed(store, project.Name),
		"another project's abort must not release this preparation")
	require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, prepared.PreparationID))

	assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name),
		"configuration that reuses the old name must not stay suppressed")
	_, err = store.RenameProject(t.Context(), project.ID, project.Name)
	require.NoError(t, err)
	require.NoError(t, rebindToTarget(t, store, credentials, project))
}

// Provider-backed and config-managed preparations durably mark the
// reservation, so abort must refuse them however the registry is keyed and
// even after a daemon restart has emptied it.
func TestAbortFederationReplicaLeaveRefusesDurablyMarkedLeave(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider bool
		restart  bool
	}{
		{name: "provider-backed preparation", provider: true},
		{name: "config-managed preparation after restart", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, project, _ := prepareFederationRebind(t, rebindSourceURL)
			credentials := newReplicaCredentialStore()
			credential := rebindCredential(rebindSourceURL, false, true)
			if tc.provider {
				credential.Provider = &config.FederationProviderCredential{
					RequestID: uuid.New(), Command: []string{"example-credential-provider"},
					Intent: "collaborate", SpokeInstanceUID: store.InstanceUID(),
					LocalProjectUID: project.UID, HubProjectUID: project.UID,
					EnrollmentID: 7, Status: "ready",
				}
			}
			credentials.put(project.UID, credential)
			prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
			require.NoError(t, err)
			if tc.restart {
				daemon.ForgetFederationReplicaTransitionsForTest(store)
			}

			err = daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, prepared.PreparationID)

			require.ErrorIs(t, err, daemon.ErrFederationReplicaLeaveStarted)
			current, found, readErr := credentials.FederationCredential(t.Context(), project.UID)
			require.NoError(t, readErr)
			require.True(t, found)
			assert.True(t, current.LeavePending, "abort must not clear the durable leave marker")
		})
	}
}

// A preparation canceled while it drains an earlier hub operation never reached
// its client, so the daemon releases it: a retried leave can then be aborted.
func TestCanceledFederationReplicaLeavePreparationIsReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, project, _ := prepareFederationRebind(t, rebindSourceURL)
		credentials := newReplicaCredentialStore()
		credential := rebindCredential(rebindSourceURL, false, false)
		credentials.put(project.UID, credential)

		cancelPrepareWhileDraining(t, store, credentials, project, credential, prepareHooks{})

		assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name),
			"a canceled preparation must not keep the mapping blocked")
		prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID, "")
		require.NoError(t, err)
		require.NoError(t, daemon.AbortFederationReplicaLeave(t.Context(), store, credentials, project.ID, prepared.PreparationID))
		assert.False(t, daemon.FederationReplicaMappingSuppressed(store, project.Name))
		require.NoError(t, rebindToTarget(t, store, credentials, project))
	})
}

// A leave that completes before or during a canceled preparation stays left,
// so configuration cannot re-create the mapping the operator left.
func TestCanceledFederationReplicaLeavePreparationKeepsCompletedLeave(t *testing.T) {
	for _, tc := range []struct {
		name          string
		whileDraining bool
	}{
		{name: "leave completed before the retried preparation"},
		{name: "leave completed while the preparation drains", whileDraining: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, project, _ := prepareFederationRebind(t, rebindSourceURL)
				credentials := newReplicaCredentialStore()
				credential := rebindCredential(rebindSourceURL, false, false)
				credentials.put(project.UID, credential)
				completeLeave := func() {
					_, err := daemon.LeaveFederationReplica(t.Context(), store, credentials, nil, project.ID)
					require.NoError(t, err)
				}
				hooks := prepareHooks{beforePrepare: completeLeave}
				if tc.whileDraining {
					hooks = prepareHooks{whileDraining: completeLeave}
				}

				cancelPrepareWhileDraining(t, store, credentials, project, credential, hooks)

				assert.True(t, daemon.FederationReplicaMappingSuppressed(store, project.Name),
					"a canceled preparation must not erase a completed leave")
			})
		})
	}
}

// prepareHooks run around the preparation that cancelPrepareWhileDraining
// cancels.
type prepareHooks struct {
	beforePrepare func()
	whileDraining func()
}

// cancelPrepareWhileDraining starts a hub operation, then cancels a leave
// preparation while it waits for that operation to drain. Call it inside a
// synctest bubble.
func cancelPrepareWhileDraining(
	t *testing.T,
	store db.Storage,
	credentials *replicaCredentialStore,
	project db.Project,
	credential config.FederationCredential,
	hooks prepareHooks,
) {
	t.Helper()
	endOperation, err := daemon.BeginFederationReplicaHubOperation(
		t.Context(), store, credentials, project.Name,
		config.FederationManagedCredentialReservation{ProjectUID: project.UID, Credential: credential},
	)
	require.NoError(t, err)
	if hooks.beforePrepare != nil {
		hooks.beforePrepare()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prepareErr := make(chan error, 1)
	go func() {
		_, err := daemon.PrepareFederationReplicaLeave(ctx, store, credentials, project.ID, "")
		prepareErr <- err
	}()
	synctest.Wait()
	require.True(t, daemon.FederationReplicaLeavePreparedForTest(store, project.Name),
		"prepare must hold its preparation while the hub operation drains")
	select {
	case err := <-prepareErr:
		require.FailNow(t, "prepare returned before the hub operation drained", "error: %v", err)
	default:
	}
	if hooks.whileDraining != nil {
		hooks.whileDraining()
	}
	cancel()
	require.ErrorIs(t, <-prepareErr, context.Canceled)
	_, err = endOperation(t.Context(), 0)
	require.NoError(t, err)
}
