package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestForwardedClaimPreservesFixedProjectScope(t *testing.T) {
	for _, tc := range []struct {
		action, scope string
	}{
		{"acquire", "source"}, {"renew", "source"}, {"release", "source"},
		{"status", "source"}, {"assignment", "source"},
		{"acquire", "both"}, {"acquire", "unlimited"},
	} {
		t.Run(tc.action+"/"+tc.scope, func(t *testing.T) {
			ctx := t.Context()
			hub, spoke, source, replica, issue, _ := createClaimForwardingPair(t, "claim")
			target, err := hub.DB.CreateProject(ctx, "scope-forward-target")
			require.NoError(t, err)
			_, err = hub.DB.EnableProjectFederation(ctx, target.ID, "tester")
			require.NoError(t, err)
			localTarget, err := spoke.DB.CreateProjectWithUID(ctx, target.Name, target.UID)
			require.NoError(t, err)
			_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: localTarget.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: target.ID, HubProjectUID: target.UID, Actor: "tester", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: "scope-forward-global", SpokeInstanceUID: spoke.DB.InstanceUID(), Capabilities: "claim,push,pull", Actor: "tester"})
			require.NoError(t, err)
			require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: source.ID, Token: grant.Token, Actor: "tester", Capabilities: "claim,push,pull"}))
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
			// The spoke has not pulled the move. The hub must apply the caller's
			// fixed boundary when it resolves this stable UID in its current home.
			stale, err := spoke.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, replica.ID, stale.ProjectID)
			if tc.action == "renew" || tc.action == "release" || tc.action == "status" {
				_, err = hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: target.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: spoke.DB.InstanceUID(), Holder: "tester", ClientKind: "cli"}, ClaimKind: "timed", TTL: 120 * time.Second, Purpose: "excluded-lease-purpose", Now: time.Now().UTC()})
				require.NoError(t, err)
			}
			beforeLease, err := hub.DB.ClaimStatusReadOnly(ctx, target.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			beforeIssue, err := hub.DB.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			headers := map[string]string{}
			if tc.scope != "unlimited" {
				uids := []string{source.UID}
				if tc.scope == "both" {
					uids = append(uids, target.UID)
				}
				raw, err := json.Marshal(uids)
				require.NoError(t, err)
				headers[api.ProjectScopeHeader] = string(raw)
			}
			method, path := http.MethodPost, claimActionPath(replica.ID, issue.UID, tc.action)
			var body any = map[string]any{"holder": "local-agent", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": 600}
			switch tc.action {
			case "status":
				method, path, body = http.MethodGet, claimStatusPath(replica.ID, issue.UID), nil
			case "assignment":
				path = fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/claim", replica.ID, issue.UID)
				body = map[string]any{"actor": "tester", "ttl_seconds": 600}
			}
			response, raw := envDoRaw(t, spoke, method, path, body, headers)
			if tc.scope == "source" {
				afterIssue, err := hub.DB.IssueByID(ctx, issue.ID)
				require.NoError(t, err)
				require.Equal(t, beforeIssue.Owner, afterIssue.Owner, "denial must precede any remote assignment")
				afterLease, err := hub.DB.ClaimStatusReadOnly(ctx, target.ID, issue.UID, time.Now().UTC())
				require.NoError(t, err)
				require.Equal(t, beforeLease.Held, afterLease.Held, "denial must precede any remote acquire/release")
				require.Equal(t, beforeLease.Claim, afterLease.Claim, "denial must precede remote renewal")
				require.Equal(t, http.StatusNotFound, response.StatusCode, string(raw))
				require.NotContains(t, string(raw), "excluded-lease-purpose")
			} else {
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				afterLease, err := hub.DB.ClaimStatusReadOnly(ctx, target.ID, issue.UID, time.Now().UTC())
				require.NoError(t, err)
				require.True(t, afterLease.Held)
			}
		})
	}
}
