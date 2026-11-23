package federation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestPushFederationQuarantineAfterHeldEdit(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, resolution := range []string{"skip", "retry"} {
			t.Run(backend+"/"+resolution, func(t *testing.T) {
				ctx := t.Context()
				var store db.Storage
				var err error
				if backend == "postgres" {
					dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
					t.Cleanup(cleanup)
					store, err = pgstore.Open(ctx, dsn)
				} else {
					store, err = sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
				}
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				project, err := store.CreateProject(ctx, "source")
				require.NoError(t, err)
				var issues []db.Issue
				for _, title := range []string{"held", "invalid"} {
					issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "worker"})
					require.NoError(t, err)
					issues = append(issues, issue)
				}
				cursor, err := store.MaxLocalOriginEventID(ctx, project.ID)
				require.NoError(t, err)
				var events []db.Event
				for _, issue := range issues {
					_, event, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "offline", Author: "worker"})
					require.NoError(t, err)
					events = append(events, event)
				}
				held, invalid := true, true
				var received []string
				polls := 0
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						polls++
						_ = json.NewEncoder(w).Encode(api.PollEventsBody{})
						return
					}
					var body api.FederationIngestEventsRequestBody
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					for _, event := range body.Events {
						if event.EventUID == events[0].UID && held {
							w.WriteHeader(http.StatusForbidden)
							_, _ = w.Write([]byte(`{"error":{"code":"federation_project_not_enrolled"}}`))
							return
						}
						if event.EventUID == events[1].UID && invalid {
							http.Error(w, "invalid event", http.StatusConflict)
							return
						}
					}
					for _, event := range body.Events {
						received = append(received, event.EventUID)
					}
					_ = json.NewEncoder(w).Encode(api.FederationIngestEventsBody{Accepted: len(body.Events), PushCursorEventID: body.Events[len(body.Events)-1].EventID})
				}))
				t.Cleanup(hub.Close)
				binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, PushEnabled: true, PushCursorEventID: cursor, Actor: "worker", Enabled: true})
				require.NoError(t, err)
				client, err := NewClient(ctx, hub.URL, "token", clientpkg.Opts{})
				require.NoError(t, err)
				require.Error(t, SyncFederationOnce(ctx, store, binding, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "token"}))
				require.Equal(t, 1, polls, "the held edit must not prevent pulling")
				q, err := store.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
				require.NoError(t, err)
				require.Equal(t, []string{events[1].UID}, q.EventUIDs)
				if resolution == "skip" {
					_, err = store.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{ID: q.ID, ProjectID: project.ID, Actor: "worker", Reason: "invalid event"})
				} else {
					_, err = store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{ID: q.ID, ProjectID: project.ID, Actor: "worker", Reason: "corrected"})
					invalid = false
				}
				require.NoError(t, err)
				binding, err = store.FederationBindingByProject(ctx, project.ID)
				require.NoError(t, err)
				require.Equal(t, cursor, binding.PushCursorEventID)
				held = false
				require.NoError(t, PushDirectSpoke(ctx, store, binding, 42, client))
				require.Contains(t, received, events[0].UID)
				if resolution == "skip" {
					require.NotContains(t, received, events[1].UID)
				} else {
					require.Contains(t, received, events[1].UID)
				}
			})
		}
	}
}
