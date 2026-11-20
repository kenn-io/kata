package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedLegacyParentUnlink(t *testing.T, store db.Storage) error {
	for _, scenario := range []string{"clear", "replace", "reverse-primary", "blocks", "unpaired"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			source, route, parent, move := federatedRehomeFixture(t, store, "legacy-"+scenario)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Child", Author: "worker"})
			require.NoError(t, err)
			_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: child.ID, FromUID: child.UID, FromShortID: child.ShortID, ToUID: parent.UID, ToShortID: parent.ShortID, Actor: "worker"})
			require.NoError(t, err)
			next, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Next parent", Author: "peer-member"})
			require.NoError(t, err)
			origin := move.OriginInstanceUID
			routeGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &route.ID, SpokeInstanceUID: origin, Capabilities: "push", Actor: "worker"})
			require.NoError(t, err)
			peerGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &source.ID, SpokeInstanceUID: origin, Capabilities: "push", Actor: "peer-member"})
			require.NoError(t, err)
			kind := "parent"
			if scenario == "blocks" {
				kind = "blocks"
			}
			payload := map[string]string{"from_uid": child.UID, "to_uid": parent.UID, "type": kind}
			if scenario == "unpaired" {
				payload["link_from_uid"] = child.UID
			}
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			primary := &child.UID
			if scenario == "reverse-primary" {
				primary = &parent.UID
			}
			at := time.Now().Add(time.Hour).UnixMilli()
			unlink := newRemoteEvent(t, route, primary, "issue.unlinked", "worker", origin, at, raw)
			params := db.FederationIngestParams{ProjectID: route.ID, FederationEnrollmentID: routeGrant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{source.UID: peerGrant.Enrollment.ID}, AuthorizeReference: func(_ context.Context, _ db.Transaction, _ db.FederationEnrollment, _ db.Project) error { return nil }, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: unlink}}}
			if scenario == "replace" {
				linkedPayload, err := json.Marshal(map[string]string{"from_uid": child.UID, "to_uid": next.UID, "type": "parent"})
				require.NoError(t, err)
				linked := newRemoteEvent(t, route, &child.UID, "issue.linked", "worker", origin, at+1, linkedPayload)
				params.Events = append(params.Events, db.FederationIngestEvent{SourceEventID: 2, Event: linked})
			}
			result, err := store.IngestFederationEvents(ctx, params)
			if scenario != "clear" && scenario != "replace" {
				require.ErrorIs(t, err, db.ErrFederationIngestValidation)
				current, err := store.ParentOf(ctx, child.ID)
				require.NoError(t, err)
				require.Equal(t, parent.ID, current.ToIssueID)
				return
			}
			require.NoError(t, err)
			require.Equal(t, len(params.Events), result.Accepted)
			stored, err := store.EventsByUIDs(ctx, route.ID, []string{unlink.EventUID})
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, unlink.ContentHash, stored[0].ContentHash)
			require.JSONEq(t, string(unlink.Payload), stored[0].Payload)
			current, err := store.ParentOf(ctx, child.ID)
			if scenario == "clear" {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, next.ID, current.ToIssueID)
			}
		})
	}
	return nil
}
