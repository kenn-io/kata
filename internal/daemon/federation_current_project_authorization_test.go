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
			if tc.mode != "missing-proof" {
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
