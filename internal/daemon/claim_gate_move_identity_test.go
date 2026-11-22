package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
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
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "retained-project")
	require.NoError(t, err)
	issue, created, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "older task", Author: "old-actor"})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"not synchronized yet"}}`))
	}))
	t.Cleanup(hub.Close)
	_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "claim-token", Actor: "new-actor", Capabilities: "claim"}))
	require.NoError(t, env.DB.AdvanceFederationPushCursor(t.Context(), project.ID, created.ID))
	response, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.UID, ""), map[string]any{"actor": "new-actor", "title": "edited task"}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)})
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	now := time.Now().UTC()
	principal := db.ClaimPrincipal{HolderInstanceUID: env.DB.InstanceUID(), Holder: "other"}
	require.NoError(t, env.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{Held: true, Holder: principal, HubNow: now, Claim: &db.IssueClaim{ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4CB", IssueUID: issue.UID, HolderInstanceUID: principal.HolderInstanceUID, Holder: principal.Holder, ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}}))
	updated, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	response, raw = envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.UID, ""), map[string]any{"actor": "new-actor", "title": "blocked edit"}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, updated.Revision)})
	assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestRetainedIssueMoveChecksHubBeforeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		existingSnapshot bool
		retiredSnapshot  bool
		noBoundary       bool
		wantStatus       int
		wantSnapshots    int
		wantAuthor       string
	}{
		{name: "hub missing", status: http.StatusNotFound, wantStatus: http.StatusOK, wantSnapshots: 1, wantAuthor: "new-actor"},
		{name: "hub known", status: http.StatusOK, wantStatus: http.StatusOK, wantAuthor: "old-actor"},
		{name: "retired snapshot", status: http.StatusOK, retiredSnapshot: true, wantStatus: http.StatusOK, wantAuthor: "old-actor"},
		{name: "offline", status: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "offline with snapshot", status: http.StatusServiceUnavailable, existingSnapshot: true, wantStatus: http.StatusOK, wantSnapshots: 1, wantAuthor: "new-actor"},
		{name: "offline without retained history", status: http.StatusServiceUnavailable, noBoundary: true, wantStatus: http.StatusOK, wantAuthor: "old-actor"},
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
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
				require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: "claim-token", Actor: "new-actor", Capabilities: "claim,push"}))
			}
			if tc.existingSnapshot || tc.retiredSnapshot {
				snapshot, _, err := env.DB.RetainedIssueSnapshot(ctx, source.ID, issue.UID)
				require.NoError(t, err)
				if tc.retiredSnapshot {
					require.NoError(t, env.DB.AcknowledgeFederationPush(ctx, source.ID, snapshot.ID, []string{snapshot.UID}))
				}
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, issue.UID, "actions/move"), map[string]any{"actor": "new-actor", "to_project_uid": target.UID}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)})
			require.Equal(t, tc.wantStatus, resp.StatusCode, string(raw))
			snapshots, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, IssueUID: issue.UID, Types: []string{"issue.snapshot"}, Limit: 10})
			require.NoError(t, err)
			require.Len(t, snapshots, tc.wantSnapshots)
			if tc.wantStatus != http.StatusOK {
				assertAPIError(t, resp.StatusCode, raw, http.StatusServiceUnavailable, "federation_offline")
				stored, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.ID, stored.ProjectID)
			} else {
				moves, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, IssueUID: issue.UID, Types: []string{"issue.moved"}, Limit: 10})
				require.NoError(t, err)
				require.Len(t, moves, 1)
				var payload struct {
					Snapshot struct {
						Author string `json:"author"`
					} `json:"snapshot"`
				}
				require.NoError(t, json.Unmarshal([]byte(moves[0].Payload), &payload))
				require.Equal(t, tc.wantAuthor, payload.Snapshot.Author)
			}
		})
	}
}

func TestRetainedSubtreeMoveUsesEachHubResult(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	source, err := env.DB.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "old-actor"})
	require.NoError(t, err)
	child, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "old-actor", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, child.UID) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"not synchronized yet"}}`))
			return
		}
		_ = json.MarshalWrite(w, api.ClaimStatusBody{HubNow: time.Now().UTC()})
	}))
	t.Cleanup(hub.Close)
	for _, project := range []db.Project{source, target} {
		_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: project.ID == source.ID})
		require.NoError(t, err)
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: "claim-token", Actor: "new-actor", Capabilities: "claim,push"}))
	}
	response, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, root.UID, "actions/move"), map[string]any{"actor": "new-actor", "to_project_uid": target.UID}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, root.Revision)})
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	for _, tc := range []struct{ uid, author string }{{root.UID, "old-actor"}, {child.UID, "new-actor"}} {
		events, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, IssueUID: tc.uid, Types: []string{"issue.moved"}, Limit: 10})
		require.NoError(t, err)
		require.Len(t, events, 1)
		var payload struct {
			Snapshot struct {
				Author string `json:"author"`
			} `json:"snapshot"`
		}
		require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
		require.Equal(t, tc.author, payload.Snapshot.Author)
	}
}
