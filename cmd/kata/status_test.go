package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestProjectedClaimStateDistinguishesLeaseAndAssignment(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	before := now.Add(-time.Minute)
	after := now.Add(time.Minute)
	owner := "alice"

	for _, tc := range []struct {
		name    string
		status  string
		owner   *string
		lease   *claimForShowCLI
		pending []pendingClaimForCLI
		want    string
	}{
		{name: "active lease", status: "open", owner: &owner, lease: &claimForShowCLI{ClaimKind: "timed", ExpiresAt: &after}, want: "active"},
		{name: "expired lease", status: "open", owner: &owner, lease: &claimForShowCLI{ClaimKind: "timed", ExpiresAt: &before}, want: "expired"},
		{name: "pending lease", status: "open", owner: &owner, pending: []pendingClaimForCLI{{Holder: "alice"}}, want: "pending"},
		{name: "assignment only", status: "open", owner: &owner, want: "assigned"},
		{name: "unassigned", status: "open", want: "unassigned"},
		{name: "closed", status: "closed", owner: &owner, want: "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, projectedClaimState(tc.status, tc.owner, tc.lease, tc.pending, now))
		})
	}
}

func TestStatusAgentReportsAuthenticatedIdentityAndAssignment(t *testing.T) {
	env, dir, pid := setupCLIWorkspaceOptions(t,
		testenv.WithAuthToken("bootstrap-token"),
		testenv.WithRequireTokenIdentity(),
	)
	const operatorToken = "operator-bearer"
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{ //nolint:gosec // test-only bearer credential
		PlaintextToken: operatorToken,
		Actor:          "operator",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	owner := "operator"
	issue, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: pid,
		Title:     "report assignment",
		Author:    "operator",
		Owner:     &owner,
	})
	require.NoError(t, err)
	t.Setenv("KATA_AUTH_TOKEN", operatorToken)

	out := runCLI(t, env, dir, "--agent", "status", issue.ShortID)

	assert.Contains(t, out, "OK status issue="+issue.ShortID+" project=kata issue_status=open revision=1")
	assert.Contains(t, out, "actor=operator actor_source=db_token auth=db_token")
	assert.Contains(t, out, "instance="+env.DB.InstanceUID())
	assert.Contains(t, out, "owner=operator claim=assigned")
	assert.NotContains(t, out, operatorToken)
}

func TestStatusAgentReportsActiveTimedLease(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "report active lease")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref, "--ttl", "30m")

	out := runCLIAs(t, env, dir, "alice", "--agent", "status", ref)

	assert.Contains(t, out, "claim=active")
	assert.Contains(t, out, "holder=alice")
	assert.Contains(t, out, "lease_kind=timed")
	assert.True(t, strings.Contains(out, "expires_at="), out)
}
