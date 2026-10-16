package daemon_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestForwardedClaimPreservesIdentityProjectAuthority(t *testing.T) {
	for _, tc := range []struct {
		action, fixed string
		member        bool
	}{
		{"acquire", "", false}, {"renew", "", false}, {"release", "", false},
		{"status", "", false}, {"assignment", "", false},
		{"acquire", "both", false}, {"status", "both", false},
		{"acquire", "", true}, {"status", "", true}, {"acquire", "source", true},
	} {
		t.Run(fmt.Sprintf("%s/fixed-%s/member-%t", tc.action, tc.fixed, tc.member), func(t *testing.T) {
			ctx := t.Context()
			hub := testenv.New(t)
			source, issue := createClaimHubIssue(t, hub)
			target, err := hub.DB.CreateProject(ctx, "effective-target")
			require.NoError(t, err)
			_, err = hub.DB.EnableProjectFederation(ctx, target.ID, "tester")
			require.NoError(t, err)
			spoke := testenv.New(t, testenv.WithAuthToken("bootstrap-example-token"), testenv.WithRequireTokenIdentity())
			replicas := make([]db.Project, 2)
			for i, project := range []db.Project{source, target} {
				replicas[i], err = spoke.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
				require.NoError(t, err)
				_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replicas[i].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "tester", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
			}
			created, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, IssueUID: issue.UID, Types: []string{"issue.created"}, Limit: 1})
			require.NoError(t, err)
			require.Len(t, created, 1)
			e := created[0]
			_, err = spoke.DB.InsertRemoteEvent(ctx, replicas[0].ID, db.RemoteEvent{EventUID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName, IssueUID: e.IssueUID, RelatedIssueUID: e.RelatedIssueUID, Type: e.Type, Actor: e.Actor, Payload: []byte(e.Payload), ContentHash: e.ContentHash, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, CreatedAt: e.CreatedAt})
			require.NoError(t, err)
			require.NoError(t, spoke.DB.MaterializeFederatedProject(ctx, replicas[0].ID))
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.DB.InstanceUID(), Capabilities: "claim,push,pull", Actor: "tester"})
			require.NoError(t, err)
			require.NoError(t, config.WriteFederationCredential(source.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: source.ID, Token: grant.Token, Actor: "tester", Capabilities: "claim,push,pull"}))
			_, _, err = spoke.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "forward-member-token", Actor: "member", AdminActor: "admin"})
			require.NoError(t, err)
			team, _, err := spoke.DB.CreateTeam(ctx, "effective-team", "admin")
			require.NoError(t, err)
			_, err = spoke.DB.SetTeamMembership(ctx, team.UID, "other-member", true, "admin")
			require.NoError(t, err)
			if tc.member {
				_, err = spoke.DB.SetTeamMembership(ctx, team.UID, "member", true, "admin")
				require.NoError(t, err)
			}
			_, _, err = spoke.DB.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: target.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
			stale, err := spoke.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, replicas[0].ID, stale.ProjectID)
			if tc.action == "renew" || tc.action == "release" || tc.action == "status" {
				_, err = hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: target.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: spoke.DB.InstanceUID(), Holder: "tester", ClientKind: "cli"}, ClaimKind: "timed", TTL: 120 * time.Second, Purpose: "private-forwarded-purpose", Now: time.Now().UTC()})
				require.NoError(t, err)
			}
			beforeLease, err := hub.DB.ClaimStatusReadOnly(ctx, target.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			beforeIssue, err := hub.DB.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			beforeCursor, err := hub.DB.MaxEventID(ctx)
			require.NoError(t, err)
			headers := bearer("forward-member-token")
			if tc.fixed != "" {
				limit := []string{source.UID}
				if tc.fixed == "both" {
					limit = append(limit, target.UID)
				}
				headers[api.ProjectScopeHeader] = requestScopeValue(t, limit...)
			}
			method, path := http.MethodPost, claimActionPath(replicas[0].ID, issue.UID, tc.action)
			var body any = map[string]any{"holder": "ignored", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": 600}
			switch tc.action {
			case "status":
				method, path, body = http.MethodGet, claimStatusPath(replicas[0].ID, issue.UID), nil
			case "assignment":
				path = fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/claim", replicas[0].ID, issue.UID)
				body = map[string]any{"actor": "ignored", "ttl_seconds": 600}
			}
			response, raw := envDoRaw(t, spoke, method, path, body, headers)
			if tc.member && tc.fixed != "source" {
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				return
			}
			afterLease, err := hub.DB.ClaimStatusReadOnly(ctx, target.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, beforeLease.Claim, afterLease.Claim, "forwarded denial must precede remote acquire/renew/release")
			afterIssue, err := hub.DB.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			require.Equal(t, beforeIssue.Owner, afterIssue.Owner, "forwarded denial must precede remote assignment")
			afterCursor, err := hub.DB.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, beforeCursor, afterCursor)
			require.Equal(t, http.StatusNotFound, response.StatusCode, string(raw))
			require.NotContains(t, string(raw), "private-forwarded-purpose")
		})
	}
}
