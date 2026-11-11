package federation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestPostgresLegacyReplayRecovery(t *testing.T) {
	for _, mode := range []string{"later-update", "moved-source"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
			t.Cleanup(cleanup)
			store, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			source, err := store.CreateProject(ctx, "source-project")
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			var binding db.FederationBinding
			for _, project := range []db.Project{source, target} {
				b, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:1", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				if project.ID == source.ID {
					binding = b
				}
			}
			issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "before reset", Author: "worker"})
			require.NoError(t, err)
			event := created
			if mode == "later-update" {
				title := "updated on later page"
				_, update, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
				require.NoError(t, err)
				event = *update
				_, err = store.ExecContext(ctx, `UPDATE issues SET title=$1 WHERE id=$2`, "before reset", issue.ID)
				require.NoError(t, err)
				require.NoError(t, store.AdvanceFederationPullCursor(ctx, source.ID, 99))
			} else {
				require.NoError(t, store.AdvanceFederationPullCursor(ctx, source.ID, 100))
				binding, err = store.FederationBindingByProject(ctx, target.ID)
				require.NoError(t, err)
				require.NoError(t, store.AdvanceFederationPullCursor(ctx, target.ID, 100))
				// The interrupted destination replay has restored the source envelope,
				// but has not yet materialized the issue or attached its local ID.
				_, err = store.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE uid=$1`, event.UID)
				require.NoError(t, err)
				_, err = store.ExecContext(ctx, `DELETE FROM issues WHERE id=$1`, issue.ID)
				require.NoError(t, err)
			}
			resetAt := time.Now().UTC().Add(time.Second)
			require.NoError(t, store.RecordFederationSyncReset(ctx, binding.ProjectID, resetAt))
			require.NoError(t, store.RecordFederationSyncPullStarted(ctx, binding.ProjectID, resetAt.Add(time.Second)))
			binding, err = store.FederationBindingByProject(ctx, binding.ProjectID)
			require.NoError(t, err)
			envelope := eventEnvelopeForSyncTest(event, 100)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{Events: []api.EventEnvelope{envelope}, NextAfterID: 101}))
			}))
			t.Cleanup(hub.Close)
			var delivered []db.Event
			err = SyncFederationPullOnlyOnceWithPulledEvents(ctx, store, binding, config.FederationCredential{HubURL: hub.URL, HubProjectID: binding.HubProjectID, Token: "token"}, clientpkg.Opts{}, func(_ int64, events []db.Event) { delivered = append(delivered, events...) })
			require.NoError(t, err)
			require.Len(t, delivered, 1)
			require.Equal(t, event.UID, delivered[0].UID)
			due, recorded, err := store.FederationLocalReplayStatus(ctx, event.UID)
			require.NoError(t, err)
			require.True(t, recorded)
			require.False(t, due)
			if mode == "later-update" {
				repaired, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, "updated on later page", repaired.Title)
			}
		})
	}
}
