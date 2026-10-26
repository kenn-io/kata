package daemon_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

// A queued batch can contain several contributors and legacy untagged events.
// Native audits caused by each source must use that source's frozen teammate,
// including clearing the unrelated context of the client sending the batch.
func TestForwardedIngestClaimAuditUsesSourceTeammate(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			var err error
			if backend == "postgres" {
				dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
				t.Cleanup(cleanup)
				store, err = pgstore.Open(t.Context(), dsn)
			} else {
				store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, err := store.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(t.Context(), project.ID, "tester")
			require.NoError(t, err)
			enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "audit-ingest-token", SpokeInstanceUID: federationTestSpokeUID, ProjectID: &project.ID, Capabilities: "push", Actor: "tester"})
			require.NoError(t, err)
			server := startTestServer(t, daemon.ServerConfig{DB: store})
			var sourceEvents []db.RemoteEvent
			var batch []any
			for i, handle := range []string{"builder-1", "reviewer-2", ""} {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Close example task", Author: "tester"})
				require.NoError(t, err)
				_, err = store.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: project.ID, IssueRef: issue.UID,
					Principal: db.ClaimPrincipal{HolderInstanceUID: federationTestSpokeUID, Holder: "other-holder", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
				require.NoError(t, err)
				eventUID, err := uid.New()
				require.NoError(t, err)
				source := claimAuditDeliveryRemoteEvent(t, project, federationTestSpokeUID, issue.UID, eventUID, "issue.closed", "tester", int64(101+i), handle)
				sourceEvents = append(sourceEvents, source)
				envelope := federationIngestEnvelope(t, int64(i+1), source)
				envelope["payload"] = json.RawMessage(source.Payload)
				batch = append(batch, envelope)
			}
			headers := bearer(enrollment.Token)
			headers["X-Kata-Teammate"] = "unrelated-sender"
			response, raw := doReq(t, server, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest", federationIngestBody(batch...), headers)
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
			for i, handle := range []string{"builder-1", "reviewer-2", ""} {
				source := sourceEvents[i]
				events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: *source.IssueUID, Types: []string{"claim.violated", "claim.released"}, Limit: 10})
				require.NoError(t, err)
				require.Len(t, events, 2)
				for _, event := range events {
					require.Equal(t, "tester", event.Actor)
					require.Equal(t, handle, event.Teammate, event.Type)
				}
				imported, err := store.EventsByUIDs(t.Context(), project.ID, []string{source.EventUID})
				require.NoError(t, err)
				require.Len(t, imported, 1)
				require.Equal(t, source.ContentHash, imported[0].ContentHash)
				require.Equal(t, string(source.Payload), imported[0].Payload)
				require.Equal(t, handle, imported[0].Teammate)
			}
		})
	}
}
