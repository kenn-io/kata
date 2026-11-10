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
