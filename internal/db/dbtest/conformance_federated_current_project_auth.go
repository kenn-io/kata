package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Fresh source-stream work can alter an issue which now belongs elsewhere;
// credential, actor, and host authority follow that current project.
func checkFederatedFreshCurrentProjectAuthority(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"missing-proof", "link-peer", "pull-only", "revoked", "wrong-actor", "wrong-spoke", "actor-denied", "host-denied", "authorized", "global-grant"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, _ := federatedRehomeFixture(t, store, "current-auth-"+kind)
			_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			sourceProjectID := &source.ID
			if kind == "global-grant" {
				sourceProjectID = nil
			}
			routeGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: sourceProjectID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			targetActor, targetOrigin, capabilities := "worker", origin, "push,pull"
			if kind == "wrong-actor" {
				targetActor = "other-worker"
			}
			if kind == "wrong-spoke" {
				targetOrigin, err = uid.New()
				require.NoError(t, err)
			}
			if kind == "pull-only" {
				capabilities = "pull"
			}
			targetGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: targetOrigin, ProjectID: &target.ID, Capabilities: capabilities, Actor: targetActor})
			require.NoError(t, err)
			if kind == "revoked" {
				require.NoError(t, store.RevokeFederationEnrollment(ctx, targetGrant.Enrollment.ID))
			}
			if kind == "actor-denied" {
				team, _, err := store.CreateTeam(ctx, "current-auth-team", "admin")
				require.NoError(t, err)
				_, err = store.SetTeamMembership(ctx, team.UID, "other-worker", true, "admin")
				require.NoError(t, err)
				_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: target.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
			}
			payload, err := json.Marshal(map[string]any{"uid": issue.UID, "title": "Fresh former-project update"})
			require.NoError(t, err)
			event := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(payload))
			if kind == "link-peer" {
				primary, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Source primary", Author: "worker"})
				require.NoError(t, err)
				payload, err = json.Marshal(map[string]any{"from_uid": primary.UID, "to_uid": issue.UID, "type": "related"})
				require.NoError(t, err)
				event = newRemoteEvent(t, source, &primary.UID, "issue.linked", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(payload))
				event.RelatedIssueUID = &issue.UID
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
			}
			denied := errors.New("current host project denied")
			currentHostChecked := false
			params := db.FederationIngestParams{ProjectID: source.ID, FederationEnrollmentID: routeGrant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
			if kind != "missing-proof" && kind != "link-peer" {
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: targetGrant.Enrollment.ID}
			}
			params.AuthorizeMove = func(_ context.Context, _ db.Transaction, grant db.FederationEnrollment, project db.Project) error {
				if project.UID == target.UID {
					currentHostChecked = true
					require.Equal(t, "worker", grant.Actor)
					if kind == "host-denied" {
						return denied
					}
				}
				return nil
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			result, err := store.IngestFederationEvents(ctx, params)
			allowed := kind == "authorized" || kind == "global-grant"
			if !allowed {
				require.Error(t, err, "fresh work requires authority for every affected issue's current project")
				if kind == "host-denied" {
					require.ErrorIs(t, err, denied)
					require.True(t, currentHostChecked)
				}
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after)
				retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, issue.Title, retained.Title)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, result.Accepted)
			require.True(t, currentHostChecked)
			changed, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, target.ID, changed.ProjectID)
			require.Equal(t, "Fresh former-project update", changed.Title)
			stored, err := store.EventsByUIDs(ctx, source.ID, []string{event.EventUID})
			require.NoError(t, err)
			require.Equal(t, event.ContentHash, stored[0].ContentHash, "authority checks preserve the original event envelope/hash")
			require.NoError(t, store.RevokeFederationEnrollment(ctx, targetGrant.Enrollment.ID))
			params.MoveSourceEnrollmentIDs = nil
			currentHostChecked = false
			duplicate, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err, "persisted duplicates do not need new current-project authority")
			require.Equal(t, 1, duplicate.Duplicates)
			require.Zero(t, duplicate.Accepted)
			require.False(t, currentHostChecked)
		})
	}
	return nil
}
