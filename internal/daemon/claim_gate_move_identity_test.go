package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestPendingMovedIssueClaimRefreshIgnoresHubShortIDCollision(t *testing.T) {
	for _, operation := range []string{"mutation", "show"} {
		for _, collisionHeld := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/collision_held=%t", operation, collisionHeld), func(t *testing.T) {
				env := testenv.New(t)
				source, err := env.DB.CreateProject(t.Context(), "source-project")
				require.NoError(t, err)
				target, err := env.DB.CreateProject(t.Context(), "target-project")
				require.NoError(t, err)
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "pending move issue", Author: "worker"})
				require.NoError(t, err)
				now := time.Now().UTC()
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/projects/42/issues/" + issue.UID + "/lease":
						// The moved issue has not reached the hub destination yet.
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"not synchronized yet"}}`))
					case "/api/v1/projects/42/issues/" + issue.ShortID + "/lease":
						// The hub already owns this short ID for a different issue
						// which the spoke has not pulled into its destination.
						status := api.ClaimStatusBody{Held: collisionHeld, HubNow: now}
						if collisionHeld {
							status.Holder = api.ClaimPrincipalOut{HolderInstanceUID: env.DB.InstanceUID(), Holder: "collision-holder", ClientKind: "cli"}
							status.Lease = &api.IssueClaimOut{ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4CC", ProjectID: 42, IssueUID: "01HZNQ7VFPK1XGD8R5MABXD4EX", HolderInstanceUID: env.DB.InstanceUID(), Holder: "collision-holder", ClientKind: "cli", ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}
						}
						_ = json.MarshalWrite(w, status)
					default:
						http.Error(w, "unexpected issue route", http.StatusBadRequest)
					}
				}))
				t.Cleanup(hub.Close)
				for _, project := range []db.Project{source, target} {
					_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
					require.NoError(t, err)
					require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "claim-token", Actor: "worker", Capabilities: "claim"}))
				}
				moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
				require.NoError(t, err)
				require.Equal(t, issue.ShortID, moved.Issue.ShortID, "the unpulled collision is absent from the local destination")
				principal := db.ClaimPrincipal{HolderInstanceUID: env.DB.InstanceUID(), Holder: "cached-holder", ClientKind: "cli"}
				require.NoError(t, env.DB.ApplyClaimStatus(t.Context(), target.ID, issue.UID, db.ClaimStatus{Held: true, Holder: principal, HubNow: now, Claim: &db.IssueClaim{ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4CB", IssueUID: issue.UID, HolderInstanceUID: principal.HolderInstanceUID, Holder: principal.Holder, ClientKind: principal.ClientKind, ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}}))
				path := issuePathRef(target.ID, issue.UID, "")
				if operation == "mutation" {
					response, raw := envDoRaw(t, env, http.MethodPatch, path, map[string]any{"actor": "worker", "title": "must remain rejected"}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, moved.NewRevision)})
					assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "claim_denied")
					stored, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, issue.Title, stored.Title)
				} else {
					response, raw := envDoRaw(t, env, http.MethodGet, path, nil, nil)
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					var body api.ShowIssueResponseBody
					require.NoError(t, json.Unmarshal(raw, &body))
					require.NotNil(t, body.Lease, "another issue's unclaimed status must not clear the moved issue's cached lease")
					require.Equal(t, "cached-holder", body.Lease.Holder)
				}
			})
		}
	}
}

func TestRetainedIssueEditAfterPushQueueDrains(t *testing.T) {
	for _, mode := range []string{"created", "restored", "moved"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			env := testenv.New(t)
			project, err := env.DB.CreateProject(ctx, "retained-project")
			require.NoError(t, err)
			issue, created, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "older task", Author: "old-actor"})
			require.NoError(t, err)
			cursor := created.ID
			if mode == "moved" {
				target, err := env.DB.CreateProject(ctx, "target-project")
				require.NoError(t, err)
				moved, err := env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "old-actor"})
				require.NoError(t, err)
				project, issue, cursor = target, moved.Issue, moved.Event.ID
			}
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"not synchronized yet"}}`))
			}))
			t.Cleanup(hub.Close)
			_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			require.NoError(t, env.DB.AdvanceFederationPushCursor(ctx, project.ID, cursor))
			if mode == "restored" {
				records, err := dbtest.CollectImportRecords(ctx, env.DB, db.ExportFilter{IncludeDeleted: true})
				require.NoError(t, err)
				env = testenv.New(t)
				require.NoError(t, env.DB.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
			}
			require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "claim-token", Actor: "new-actor", Capabilities: "claim,pull"}))
			response, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.UID, ""), map[string]any{"actor": "new-actor", "title": "edited task"}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)})
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		})
	}
}

func TestRetainedIssueMoveChecksHubBeforeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		status                int
		existingSnapshot      bool
		acceptedSnapshot      bool
		rejoin                bool
		noBoundary            bool
		wantNoSnapshotMessage bool
		wantStatus            int
		wantSnapshots         int
	}{
		{name: "hub missing", status: http.StatusNotFound, wantStatus: http.StatusServiceUnavailable, wantSnapshots: 0},
		{name: "hub known without recovery snapshot", status: http.StatusOK, wantStatus: http.StatusConflict, wantNoSnapshotMessage: true},
		{name: "hub known with accepted snapshot", status: http.StatusOK, existingSnapshot: true, acceptedSnapshot: true, wantStatus: http.StatusOK, wantSnapshots: 1},
		{name: "hub known after second rejoin", status: http.StatusOK, existingSnapshot: true, acceptedSnapshot: true, rejoin: true, wantStatus: http.StatusOK, wantSnapshots: 1},
		{name: "pending snapshot after second rejoin", status: http.StatusOK, existingSnapshot: true, rejoin: true, wantStatus: http.StatusServiceUnavailable, wantSnapshots: 1},
		{name: "offline", status: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "offline with accepted snapshot", status: http.StatusServiceUnavailable, existingSnapshot: true, acceptedSnapshot: true, wantStatus: http.StatusServiceUnavailable, wantSnapshots: 1},
		{name: "hub known with pending snapshot", status: http.StatusOK, existingSnapshot: true, wantStatus: http.StatusServiceUnavailable, wantSnapshots: 1},
		{name: "offline without retained history", status: http.StatusServiceUnavailable, noBoundary: true, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := t.Context()
			source, err := env.DB.CreateProject(ctx, "source-project")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "retained", Author: "old-actor"})
			require.NoError(t, err)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch tc.status {
				case http.StatusOK:
					_ = json.MarshalWrite(w, api.ClaimStatusBody{HubNow: time.Now().UTC()})
				case http.StatusNotFound:
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"not synchronized yet"}}`))
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"code":"federation_offline","message":"offline"}}`))
				}
			}))
			t.Cleanup(hub.Close)
			for _, project := range []db.Project{source, target} {
				_, err := env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: project.ID == source.ID && !tc.noBoundary})
				require.NoError(t, err)
				require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: "claim-token", Actor: "new-actor", Capabilities: "claim,push,pull"}))
			}
			if tc.existingSnapshot {
				snapshot, _, err := env.DB.RetainedIssueSnapshot(ctx, source.ID, issue.UID)
				require.NoError(t, err)
				if tc.acceptedSnapshot {
					require.NoError(t, env.DB.AdvanceFederationPushCursor(ctx, source.ID, snapshot.ID))
				}
			}
			if tc.rejoin {
				binding, err := env.DB.FederationBindingByProject(ctx, source.ID)
				require.NoError(t, err)
				_, err = env.DB.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
				_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: source.ID, HubProjectUID: source.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
				require.NoError(t, err)
				require.NoError(t, env.DB.AdvanceFederationPushCursor(ctx, source.ID, binding.PushCursorEventID))
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, issue.UID, "actions/move"), map[string]any{"actor": "new-actor", "to_project_uid": target.UID}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)})
			require.Equal(t, tc.wantStatus, resp.StatusCode, string(raw))
			snapshots, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, IssueUID: issue.UID, Types: []string{"issue.snapshot"}, Limit: 10})
			require.NoError(t, err)
			require.Len(t, snapshots, tc.wantSnapshots)
			if tc.wantStatus != http.StatusOK {
				code := "federation_offline"
				if tc.wantStatus == http.StatusConflict {
					code = "federated_move_unsupported"
				}
				errorEnvelope := assertAPIError(t, resp.StatusCode, raw, tc.wantStatus, code)
				if tc.wantNoSnapshotMessage {
					require.Contains(t, errorEnvelope.Error.Message, "no accepted recovery snapshot")
				}
				stored, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.ID, stored.ProjectID)

			} else {
				moves, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, IssueUID: issue.UID, Types: []string{"issue.moved"}, Limit: 10})
				require.NoError(t, err)
				require.Len(t, moves, 1)
			}
		})
	}
}
