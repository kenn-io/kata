package federation_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

func TestRelaySavedScheduleRequiresPeerSupport(t *testing.T) {
	for _, push := range []bool{true, false} {
		name := "pull"
		if push {
			name = "push"
		}
		t.Run(name, func(t *testing.T) {
			root := newRecoveryRoot(t, "sqlite")
			spoke := newRelayMatrixNode(t, "sqlite", "spoke-member")
			enrollRelayMatrixReplica(t, root, spoke, "spoke-alias", false)
			syncRelayMatrixNode(t, spoke)

			sender, receiver := root, spoke
			if push {
				sender, receiver = spoke, root
			}
			saved, err := metadata.NewScheduleValue("2026-11-12", "Europe/Paris")
			require.NoError(t, err)
			issue, _, err := sender.store.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: sender.project.ID, Title: "Saved date", Author: sender.account,
				Metadata: map[string]jsontext.Value{"scheduled_on": saved},
			})
			require.NoError(t, err)

			handler := *root.handler.Load()
			oldPeer := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if push && strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, r)
					response.Header().Del(db.EventFeaturesHeader)
					for key, values := range response.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(response.Code)
					_, _ = w.Write(response.Body.Bytes())
					return
				}
				if !push {
					r.Header.Del(db.EventFeaturesHeader)
				}
				handler.ServeHTTP(w, r)
			}))
			root.handler.Store(&oldPeer)
			binding, err := spoke.store.FederationBindingByProject(t.Context(), spoke.project.ID)
			require.NoError(t, err)
			err = federation.SyncFederationOnce(t.Context(), spoke.store, binding, spoke.credential)
			if push {
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			} else {
				requireUnsupportedRelayFeature(t, err)
			}
			_, err = receiver.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)

			root.handler.Store(&handler)
			syncRelayMatrixNode(t, spoke)
			got, err := receiver.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.JSONEq(t, string(issue.Metadata), string(got.Metadata))
		})
	}
}

func TestRelaySavedScheduleCheckpointRequiresPeerSupport(t *testing.T) {
	ctx := t.Context()
	root := newRecoveryRoot(t, "sqlite")
	spoke := newRelayMatrixNode(t, "sqlite", "spoke-member")
	enrollRelayMatrixReplica(t, root, spoke, "spoke-alias", false)
	binding, err := spoke.store.FederationBindingByProject(ctx, spoke.project.ID)
	require.NoError(t, err)
	saved, err := metadata.NewScheduleValue("2026-11-12", "Europe/Paris")
	require.NoError(t, err)
	_, _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{
		ProjectID: root.project.ID, Title: "Saved date", Author: root.account,
		Metadata: map[string]jsontext.Value{"scheduled_on": saved},
	})
	require.NoError(t, err)
	client, err := federation.NewClient(ctx, root.http.URL, spoke.credential.Token, clientpkg.Opts{})
	require.NoError(t, err)
	handler := *root.handler.Load()
	oldPeer := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(db.EventFeaturesHeader)
		handler.ServeHTTP(w, r)
	}))

	t.Run("download", func(t *testing.T) {
		root.handler.Store(&oldPeer)
		t.Cleanup(func() { root.handler.Store(&handler) })
		_, err := client.RelayReset(t.Context(), root.project.ID)
		requireUnsupportedRelayFeature(t, err)
		checkpointKey := db.RelayResetMetadataPrefix + root.project.UID + "." + binding.RelayConfig.BindingUID
		for row, err := range root.store.ExportMeta(t.Context()) {
			require.NoError(t, err)
			require.NotEqual(t, checkpointKey, row.Key, "incompatible download must roll back checkpoint creation")
		}
	})
	checkpoint, err := client.RelayReset(ctx, root.project.ID)
	require.NoError(t, err)
	epoch := checkpoint.Translation.Authority.Epoch
	require.Greater(t, epoch, binding.RelayConfig.ResetEpoch)

	t.Run("activation", func(t *testing.T) {
		root.handler.Store(&oldPeer)
		t.Cleanup(func() { root.handler.Store(&handler) })
		_, err := client.OfferRelayDeliveries(t.Context(), root.project.ID, db.RelayStreamReceipt, 1, epoch)
		requireUnsupportedRelayFeature(t, err)
		meta, err := client.ProjectFederation(t.Context(), root.project.ID)
		require.NoError(t, err)
		require.Equal(t, binding.RelayConfig.ResetEpoch, meta.Relay.ResetEpoch)
	})
	_, err = client.OfferRelayDeliveries(ctx, root.project.ID, db.RelayStreamReceipt, 1, epoch)
	require.NoError(t, err)
	meta, err := client.ProjectFederation(ctx, root.project.ID)
	require.NoError(t, err)
	require.Equal(t, epoch, meta.Relay.ResetEpoch)
}

func requireUnsupportedRelayFeature(t *testing.T, err error) {
	t.Helper()
	var status *federation.HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusConflict, status.HTTPStatus())
	require.Contains(t, status.Body, "unsupported_event_features")
}

func TestRelayFutureScheduleRulesStayRetryable(t *testing.T) {
	for _, direction := range []string{"push", "pull"} {
		t.Run(direction, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, "sqlite")
			spoke := newRelayMatrixNode(t, "sqlite", "spoke-member")
			enrollRelayMatrixReplica(t, root, spoke, "spoke-alias", false)
			sender, receiver := spoke, root
			if direction == "pull" {
				sender, receiver = root, spoke
			}
			issue, created, err := sender.store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: sender.project.ID, Title: "Future schedule", Author: sender.account,
			})
			require.NoError(t, err)
			syncRelayMatrixNode(t, spoke)
			binding, err := spoke.store.FederationBindingByProject(ctx, spoke.project.ID)
			require.NoError(t, err)

			source := db.RemoteEventFromStored(created)
			source.EventUID, err = uid.New()
			require.NoError(t, err)
			source.Type = "issue.metadata_updated"
			source.HLCCounter++
			futureVersion := "sha256:" + strings.Repeat("f", 64)
			source.Payload = jsontext.Value(fmt.Sprintf(`{"issue_uid":%q,"diff":{"scheduled_on":{"from":null,"to":{"value":"2026-11-12","timezone":"Europe/Paris","tzdata_version":%q}}}}`, issue.UID, futureVersion))
			source.ContentHash, err = db.RemoteEventContentHash(source)
			require.NoError(t, err)
			inserted, err := sender.store.InsertRemoteEvent(ctx, sender.project.ID, source)
			require.NoError(t, err)
			require.True(t, inserted)

			pending, err := sender.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Equal(t, source.EventUID, pending[0].SourceUID)
			for range 2 {
				err = federation.SyncFederationOnce(ctx, spoke.store, binding, spoke.credential)
				active, listErr := spoke.store.ActiveFederationQuarantinesByProject(ctx, spoke.project.ID)
				require.NoError(t, listErr)
				require.Empty(t, active, "version skew must keep sync retryable")
				if direction == "push" {
					requireUnsupportedRelayFeature(t, err)
				} else {
					require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				}
			}
			retained, err := sender.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Equal(t, pending, retained, "the rejected prefix remains unacknowledged")
			got, err := receiver.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotContains(t, got.Metadata, "scheduled_on", "the rejected edit does not reach the projection")
		})
	}
}
