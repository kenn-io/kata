package dbtest

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveAcknowledgementRetry(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"missing-proof", "revoked", "archived", "disabled", "actor-denied"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, event := federatedRehomeFixture(t, store, "ack-retry-"+kind)
			sourceGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: event.OriginInstanceUID, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			targetGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: event.OriginInstanceUID, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			params := db.FederationIngestParams{ProjectID: target.ID, FederationEnrollmentID: targetGrant.Enrollment.ID, SpokeInstanceUID: event.OriginInstanceUID, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{source.UID: sourceGrant.Enrollment.ID}, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
			authCtx := db.WithTransactionFence(ctx, store.FederationEnrollmentTransactionFence(targetGrant.Enrollment, target.ID, "push"))
			accepted, err := store.IngestFederationEvents(authCtx, params)
			require.NoError(t, err)
			require.Equal(t, 1, accepted.Accepted)
			switch kind {
			case "missing-proof":
				params.MoveSourceEnrollmentIDs = nil
			case "revoked":
				require.NoError(t, store.RevokeFederationEnrollment(ctx, sourceGrant.Enrollment.ID))
			case "archived":
				_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "worker", Force: true})
				require.NoError(t, err)
			case "disabled":
				binding, err := store.FederationBindingByProject(ctx, source.ID)
				require.NoError(t, err)
				binding.Enabled = false
				_, err = store.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
			case "actor-denied":
				team, _, err := store.CreateTeam(ctx, "ack-retry-team", "admin")
				require.NoError(t, err)
				_, err = store.SetTeamMembership(ctx, team.UID, "other-worker", true, "admin")
				require.NoError(t, err)
				_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: source.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			hostChecked := false
			params.AuthorizeMove = func(context.Context, db.Transaction, db.FederationEnrollment, db.Project) error {
				hostChecked = true
				return errors.New("old source host authority no longer available")
			}
			replayed, err := store.IngestFederationEvents(authCtx, params)
			require.NoError(t, err, "acknowledging committed history must not need today's source authority")
			require.Equal(t, 1, replayed.Duplicates)
			require.Zero(t, replayed.Accepted)
			require.Equal(t, int64(1), replayed.PushCursorEventID)
			require.False(t, hostChecked)
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, target.ID, retained.ProjectID)

			changed := event
			var payload map[string]any
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			payload["to_short_id"] = "changed-short"
			changed.Payload, err = json.Marshal(payload)
			require.NoError(t, err)
			changed.ContentHash, err = db.RemoteEventContentHash(changed)
			require.NoError(t, err)
			params.Events[0].Event = changed
			_, err = store.IngestFederationEvents(authCtx, params)
			require.ErrorIs(t, err, db.ErrRemoteEventConflict, "accepted UID never authorizes altered content")
			changed = event
			changed.ContentHash = "invalid"
			params.Events[0].Event = changed
			_, err = store.IngestFederationEvents(authCtx, params)
			require.ErrorIs(t, err, db.ErrRemoteEventHashMismatch)
			fresh := event
			fresh.EventUID, err = uid.New()
			require.NoError(t, err)
			fresh.ContentHash, err = db.RemoteEventContentHash(fresh)
			require.NoError(t, err)
			params.Events[0].Event = fresh
			_, err = store.IngestFederationEvents(authCtx, params)
			require.Error(t, err, "fresh moves still require live source authority")
			params.Events[0].Event = event
			require.NoError(t, store.RevokeFederationEnrollment(ctx, targetGrant.Enrollment.ID))
			_, err = store.IngestFederationEvents(authCtx, params)
			require.ErrorIs(t, err, db.ErrNotFound, "route enrollment remains mandatory for duplicates")
		})
	}
	return nil
}
