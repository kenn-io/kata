package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestFederationSupplementalAuthorityPreservesRequestScope(t *testing.T) {
	for _, effect := range []string{"move", "historical-update"} {
		for _, withHost := range []bool{false, true} {
			for _, scope := range []string{"unrestricted", "both-projects", "route-only"} {
				t.Run(fmt.Sprintf("%s/host=%t/%s", effect, withHost, scope), func(t *testing.T) {
					access := &moveSourceHostAccess{mode: "allow"}
					var options []testenv.Option
					if withHost {
						options = append(options, func(cfg *daemon.ServerConfig) { cfg.HostFederationAccess = access })
					}
					env := testenv.New(t, options...)
					source, target, issue := seedMovePair(t, env)
					for _, project := range []db.Project{source, target} {
						_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
						require.NoError(t, err)
					}
					origin, err := uid.New()
					require.NoError(t, err)
					sourceGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"})
					require.NoError(t, err)
					targetGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "tester"})
					require.NoError(t, err)
					route, supplemental := target, source
					routeToken, supplementalToken := targetGrant.Token, sourceGrant.Token
					eventType := "issue.moved"
					payload := map[string]any{
						"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID,
						"to_project_uid": target.UID, "to_short_id": issue.ShortID,
						"snapshot": map[string]string{"uid": issue.UID, "title": issue.Title, "author": "tester", "short_id": issue.ShortID, "status": "open"},
					}
					if effect == "historical-update" {
						_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
						require.NoError(t, err)
						route, supplemental = source, target
						routeToken, supplementalToken = sourceGrant.Token, targetGrant.Token
						eventType = "issue.updated"
						payload = map[string]any{"uid": issue.UID, "title": "Authorized delayed edit"}
					}
					access.sourceUID = supplemental.UID
					encoded, err := json.Marshal(payload)
					require.NoError(t, err)
					eventUID, err := uid.New()
					require.NoError(t, err)
					at := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
					remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: route.UID, ProjectName: route.Name, IssueUID: &issue.UID, Type: eventType, Actor: "tester", Payload: encoded, HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
					remote.ContentHash, err = db.RemoteEventContentHash(remote)
					require.NoError(t, err)
					body := federationIngestBody(federationIngestEnvelope(t, 1, remote))
					body["move_source_tokens"] = map[string]string{supplemental.UID: supplementalToken}
					headers := bearer(routeToken)
					headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
					switch scope {
					case "both-projects":
						headers[requestProjectScopeHeader] = requestScopeValue(t, source.UID, target.UID)
					case "route-only":
						headers[requestProjectScopeHeader] = requestScopeValue(t, route.UID)
					}
					before, err := env.DB.MaxEventID(t.Context())
					require.NoError(t, err)
					response, raw := envDoRaw(t, env, http.MethodPost, projectPath(route.ID)+"/federation/events:ingest", body, headers)
					got, err := env.DB.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					if scope == "route-only" {
						require.Equal(t, http.StatusForbidden, response.StatusCode, string(raw))
						owner := source.ID
						if effect == "historical-update" {
							owner = target.ID
						}
						require.Equal(t, owner, got.ProjectID)
						require.Equal(t, issue.Title, got.Title)
						cursor, err := env.DB.MaxEventID(t.Context())
						require.NoError(t, err)
						require.Equal(t, before, cursor, "scope denial rolls back the entire incoming event")
						require.Zero(t, access.sourceChecks.Load(), "fixed scope rejects before supplemental host admission")
						return
					}
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					require.Equal(t, target.ID, got.ProjectID)
					if effect == "historical-update" {
						require.Equal(t, "Authorized delayed edit", got.Title)
					}
				})
			}
		}
	}
}
