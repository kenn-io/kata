package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestFederationFreshCurrentProjectHostAuthority(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		status int
	}{
		{"allow", 200}, {"missing-proof", 403}, {"admission-denied", 403},
		{"fence-denied", 403}, {"missing-fence", 503}, {"limited", 429},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			access := &moveSourceHostAccess{mode: tc.mode}
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.HostFederationAccess = access })
			source, target, issue := seedMovePair(t, env)
			for _, project := range []db.Project{source, target} {
				_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
				require.NoError(t, err)
			}
			_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
			access.sourceUID = target.UID
			origin, err := uid.New()
			require.NoError(t, err)
			routeGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"})
			require.NoError(t, err)
			currentGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "tester"})
			require.NoError(t, err)
			eventUID, err := uid.New()
			require.NoError(t, err)
			at := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
			payload, err := json.Marshal(map[string]string{"uid": issue.UID, "title": "Authorized current-project update"})
			require.NoError(t, err)
			remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &issue.UID, Type: "issue.updated", Actor: "tester", Payload: jsontext.Value(payload), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
			remote.ContentHash, err = db.RemoteEventContentHash(remote)
			require.NoError(t, err)
			body := api.FederationIngestEventsRequestBody{SchemaVersion: db.CurrentSchemaVersion(), Events: []api.FederationIngestEventEnvelope{{EventID: 1, EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &issue.UID, Type: remote.Type, Actor: remote.Actor, Payload: remote.Payload, HLCPhysicalMS: remote.HLCPhysicalMS, ContentHash: remote.ContentHash, CreatedAt: at}}}
			if tc.mode != "missing-proof" {
				body.MoveSourceTokens = map[string]string{target.UID: currentGrant.Token}
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			access.afterRollback = func(ctx context.Context) error {
				retained, err := env.DB.IssueByUID(db.WithAuthorizedProjects(ctx, nil), issue.UID, db.IncludeDeletedNo)
				if err == nil {
					require.Equal(t, issue.Title, retained.Title)
				}
				return err
			}
			headers := bearer(routeGrant.Token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			response, raw := envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
			require.Equalf(t, tc.status, response.StatusCode, "%s", raw)
			if tc.mode == "missing-proof" {
				// The route credential is valid; the spoke lacks the issue's
				// current project. The error names only the issue, which the
				// spoke already holds, and not the project it has not joined.
				envelope := assertAPIError(t, response.StatusCode, raw, http.StatusForbidden, "federation_project_not_enrolled")
				require.Contains(t, envelope.Error.Message, issue.UID)
				require.NotContains(t, string(raw), target.Name)
				require.NotContains(t, string(raw), target.UID)
				require.NotContains(t, envelope.Error.Message, "credential is not currently authorized")
			} else {
				require.Positive(t, access.sourceChecks.Load())
			}
			if tc.mode == "fence-denied" {
				require.True(t, access.finished.Load())
			}
			if tc.mode != "allow" {
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
				return
			}
			require.NoError(t, env.DB.RevokeFederationEnrollment(t.Context(), currentGrant.Enrollment.ID))
			access.mode = "admission-denied"
			access.sourceChecks.Store(0)
			response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
			require.Equalf(t, http.StatusOK, response.StatusCode, "already accepted history survives stale supplemental proofs: %s", raw)
			require.Zero(t, access.sourceChecks.Load())
		})
	}
}

func TestFederationIngestPartialNotEnrolledHTTP(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	source, target, moved := seedMovePair(t, env)
	for _, project := range []db.Project{source, target} {
		_, err := env.DB.EnableProjectFederation(ctx, project.ID, "tester")
		require.NoError(t, err)
	}
	stayingA, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Staying A", Author: "tester"})
	require.NoError(t, err)
	stayingB, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Staying B", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: moved.Revision, Actor: "tester",
	})
	require.NoError(t, err)

	origin, err := uid.New()
	require.NoError(t, err)
	routeGrant, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester",
	})
	require.NoError(t, err)

	makeEnvelope := func(sourceID int64, issueUID, title string, offset time.Duration) api.FederationIngestEventEnvelope {
		t.Helper()
		eventUID, err := uid.New()
		require.NoError(t, err)
		at := time.Now().UTC().Add(time.Hour + offset).Truncate(time.Millisecond)
		payload, err := json.Marshal(map[string]string{"uid": issueUID, "title": title})
		require.NoError(t, err)
		remote := db.RemoteEvent{
			EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name,
			IssueUID: &issueUID, Type: "issue.updated", Actor: "tester",
			Payload: jsontext.Value(payload), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at,
		}
		remote.ContentHash, err = db.RemoteEventContentHash(remote)
		require.NoError(t, err)
		return api.FederationIngestEventEnvelope{
			EventID: sourceID, EventUID: eventUID, OriginInstanceUID: origin,
			ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &issueUID,
			Type: remote.Type, Actor: remote.Actor, Payload: remote.Payload,
			HLCPhysicalMS: remote.HLCPhysicalMS, ContentHash: remote.ContentHash, CreatedAt: at,
		}
	}

	evBefore := makeEnvelope(1, stayingA.UID, "Before moved", time.Second)
	evMoved := makeEnvelope(2, moved.UID, "Blocked moved", 2*time.Second)
	evAfter := makeEnvelope(3, stayingB.UID, "After moved", 3*time.Second)

	headers := bearer(routeGrant.Token)
	headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
	mixedBody := api.FederationIngestEventsRequestBody{
		SchemaVersion: db.CurrentSchemaVersion(),
		Events:        []api.FederationIngestEventEnvelope{evBefore, evMoved, evAfter},
	}

	// Without partial_not_enrolled, the whole batch fails with 403 and writes nothing.
	resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", mixedBody, headers)
	envelope := assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, db.FederationProjectNotEnrolledCode)
	require.Contains(t, envelope.Error.Message, moved.UID)
	require.NotContains(t, string(raw), target.Name)
	require.NotContains(t, string(raw), target.UID)
	checkA, err := env.DB.IssueByUID(ctx, stayingA.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Staying A", checkA.Title)

	// With partial_not_enrolled=true, unrelated events are accepted and only the moved issue event is refused.
	resp, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest?partial_not_enrolled=true", mixedBody, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var ack api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &ack))
	require.Equal(t, 2, ack.Accepted)
	require.Zero(t, ack.Duplicates)
	require.EqualValues(t, 1, ack.PushCursorEventID)
	require.Len(t, ack.RefusedEvents, 1)
	require.Equal(t, int64(2), ack.RefusedEvents[0].EventID)
	require.Equal(t, evMoved.EventUID, ack.RefusedEvents[0].EventUID)
	require.Equal(t, db.FederationProjectNotEnrolledCode, ack.RefusedEvents[0].Code)
	require.Equal(t, moved.UID, ack.RefusedEvents[0].IssueUID)
	require.NotContains(t, string(raw), target.Name)
	require.NotContains(t, string(raw), target.UID)

	checkA, err = env.DB.IssueByUID(ctx, stayingA.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Before moved", checkA.Title)
	checkB, err := env.DB.IssueByUID(ctx, stayingB.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "After moved", checkB.Title)
	checkMoved, err := env.DB.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, moved.Title, checkMoved.Title)

	// With partial_not_enrolled=true and all events refused, the hub returns 200 with refused_events populated.
	allRefusedBody := api.FederationIngestEventsRequestBody{
		SchemaVersion: db.CurrentSchemaVersion(),
		Events:        []api.FederationIngestEventEnvelope{evMoved},
	}
	resp, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest?partial_not_enrolled=true", allRefusedBody, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var allRefusedAck api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &allRefusedAck))
	require.Zero(t, allRefusedAck.Accepted)
	require.Zero(t, allRefusedAck.PushCursorEventID)
	require.Len(t, allRefusedAck.RefusedEvents, 1)
	require.Equal(t, evMoved.EventUID, allRefusedAck.RefusedEvents[0].EventUID)
}
