package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedCronRunIssueAuthority(t *testing.T, store db.Storage) error {
	for _, eventType := range []string{"cron.run.observed", "cron.run.snapshot"} {
		for _, kind := range []string{"same-project", "moved-authorized", "moved-missing-proof", "moved-actor-denied", "unreferenced-run"} {
			t.Run(eventType+"/"+kind, func(t *testing.T) {
				ctx := t.Context()
				source, target, issue, _ := federatedRehomeFixture(t, store, "cron-auth-"+eventType+"-"+kind)
				job, _ := nativeFederationDefinitions(t, store, source)
				if kind != "same-project" {
					_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
					require.NoError(t, err)
				}
				if kind == "moved-actor-denied" {
					team, _, err := store.CreateTeam(ctx, "cron-auth-team-"+eventType, "admin")
					require.NoError(t, err)
					_, err = store.SetTeamMembership(ctx, team.UID, "other-worker", true, "admin")
					require.NoError(t, err)
					_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: target.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
					require.NoError(t, err)
				}
				origin, err := uid.New()
				require.NoError(t, err)
				routeGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				targetGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				runUID, err := uid.New()
				require.NoError(t, err)
				if kind == "unreferenced-run" {
					runUID = issue.UID
				}
				at := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
				run := db.CronRun{UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &issue.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
				if kind == "unreferenced-run" {
					run.IssueUID = nil
				}
				body := db.NewCronRunObservation(run, source.UID)
				if eventType == "cron.run.snapshot" {
					body.ObservationEventUID, err = uid.New()
					require.NoError(t, err)
					body.ObservationHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: at.UnixMilli(), OriginInstanceUID: origin}
				}
				payload, err := json.Marshal(body)
				require.NoError(t, err)
				event := newRemoteEvent(t, source, nil, eventType, "worker", origin, at.UnixMilli(), payload)
				params := db.FederationIngestParams{ProjectID: source.ID, FederationEnrollmentID: routeGrant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
				if kind != "moved-missing-proof" && kind != "unreferenced-run" {
					params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: targetGrant.Enrollment.ID}
				}
				before, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				result, err := store.IngestFederationEvents(ctx, params)
				switch kind {
				case "moved-missing-proof", "moved-actor-denied":
					if kind == "moved-missing-proof" {
						require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
					} else {
						require.ErrorIs(t, err, db.ErrNotFound)
					}
					after, err := store.MaxEventID(ctx)
					require.NoError(t, err)
					require.Equal(t, before, after)
				default:
					require.NoError(t, err, "run UID and referenced issue UID are different identities")
					require.Equal(t, 1, result.Accepted)
					retained, err := store.CronRun(ctx, source.ID, runUID)
					require.NoError(t, err)
					require.Equal(t, run.IssueUID, retained.IssueUID)
					require.Equal(t, "running", retained.Status)
				}
			})
		}
	}
	return nil
}
