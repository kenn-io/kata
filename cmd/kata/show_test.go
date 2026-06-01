package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestShow_RendersLabelsAndLinksSections(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "parent")
	child := createIssue(t, env, pid, "child")
	// Two labels so we exercise the comma-join.
	for _, label := range []string{"bug", "priority:high"} {
		runCLI(t, env, dir, "label", "add", child, label)
	}
	createLinkViaHTTP(t, env, pid, child, "parent", parent)

	out := runCLI(t, env, dir, "show", child)
	// Exact section headers and comma-joined label rendering.
	assert.Contains(t, out, "--- labels ---")
	assert.Contains(t, out, "bug, priority:high")
	// Links section: viewer (child) is on the "from" side of (from=child parent to=parent)
	// so it reads "parent: <parent_short_id>" — its parent is the parent issue.
	assert.Contains(t, out, "--- links ---")
	assert.Contains(t, out, "parent: "+parent)
}

func TestShow_AgentOutputRendersIssueBodyLabelsAndComments(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	created := postJSON[createResp](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{
			"actor":    "tester",
			"title":    "Safari callback",
			"body":     "Safari can double-submit the callback.",
			"priority": int64(2),
			"labels":   []string{"bug", "safari"},
		})
	runCLI(t, env, dir, "--as", "tester", "comment", created.Issue.ShortID, "--body", "Reproduced on macOS.")

	out := runCLI(t, env, dir, "--agent", "show", created.Issue.ShortID)

	assert.Contains(t, out, "OK show "+created.Issue.ShortID+"\n")
	assert.Contains(t, out, "Issue: "+created.Issue.ShortID+" \"Safari callback\"\n")
	assert.Contains(t, out, "Status: open\n")
	assert.Contains(t, out, "Labels: bug,safari\n")
	assert.Contains(t, out, "Priority: 2\n")
	assert.Contains(t, out, "Body:\n```text\nSafari can double-submit the callback.\n```\n")
	assert.Regexp(t, regexp.MustCompile(`(?m)^- author=tester created_at=[^ \n]+$`), out)
	assert.Contains(t, out, "\n```text\nReproduced on macOS.\n```")
	assert.NotContains(t, out, "Owner:")
}

func TestShow_AgentOutputLinkRowsUseExistingLinkResponseFields(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked title")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "--agent", "show", blocker)

	assert.Contains(t, out, "Links:\n")
	assert.Contains(t, out, "- type=blocks issue="+blocked)
	assert.NotContains(t, out, `title="blocked title"`)
}

func TestShow_AgentOutputLinkRowsUsePOVLabels(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "--agent", "show", blocked)

	assert.Contains(t, out, "- type=blocked-by issue="+blocker)
	assert.NotContains(t, out, "- type=blocks issue="+blocker)
}

func TestShow_NoClaimLineOnNonFederatedUnclaimedIssue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "plain issue")

	out := runCLI(t, env, dir, "show", ref)
	assert.NotContains(t, out, "lease:")
}

func TestShow_ActiveHardClaimRendersOneClaimLine(t *testing.T) {
	env, dir, _, ref := setupFederatedHubIssue(t, "claimed issue")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)

	out := runCLI(t, env, dir, "show", ref)

	lines := leaseLines(out)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "lease: alice from instance ")
	assert.Contains(t, lines[0], "(hard)")
}

func TestShow_PendingClaimsRenderPendingNewestFirst(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "pending issue")
	oldAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	newAt := oldAt.Add(2 * time.Minute)
	enqueueCLIPendingClaim(t, env.DB, pid, ref, "oldest", oldAt)
	enqueueCLIPendingClaim(t, env.DB, pid, ref, "newest", newAt)

	out := runCLI(t, env, dir, "show", ref)

	lines := leaseLines(out)
	require.Equal(t, []string{
		"lease: newest pending",
		"lease: oldest pending",
	}, lines)
}

func TestShow_UnreachableHubRendersCachedTimedClaimAndPending(t *testing.T) {
	ctx := context.Background()
	env, dir, pid := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "cached claim")
	enableSpokeClaims(t, env, pid)
	hubNow := time.Now().UTC().Add(5 * time.Minute)
	expiresAt := hubNow.Add(29*time.Minute + 30*time.Second)
	require.NoError(t, env.DB.ApplyClaimStatus(ctx, pid, created.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached",
			ClientKind:        "cli",
		},
		Claim: &db.IssueClaim{
			ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CC",
			ProjectID:         pid,
			IssueID:           created.ID,
			IssueUID:          created.UID,
			Holder:            "cached",
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			ClientKind:        "cli",
			ClaimKind:         "timed",
			AcquiredAt:        hubNow.Add(-time.Minute),
			ExpiresAt:         &expiresAt,
			Revision:          1,
			UpdatedAt:         hubNow,
		},
		HubNow: hubNow,
	}))
	enqueueCLIPendingClaim(t, env.DB, pid, created.ShortID, "pending", hubNow.Add(time.Minute))

	out := runCLI(t, env, dir, "show", created.ShortID)

	lines := leaseLines(out)
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "lease: cached from instance 01HZNQ7VFPK1XGD8R5MABCD4CD (timed, ")
	assert.Contains(t, lines[0], " left)")
	assert.Equal(t, "lease: pending pending", lines[1])
}

func TestShow_TimedClaimUsesClaimHubNowForTimeLeft(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "fresh hub claim")
	hubNow := time.Now().UTC().Add(5 * time.Minute)
	expiresAt := hubNow.Add(29*time.Minute + 30*time.Second)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer claim-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/projects/42/issues/"+created.ShortID+"/lease", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EF",
				Holder:            "hub-alice",
				ClientKind:        "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4EE",
				ProjectID:         42,
				IssueUID:          created.UID,
				Holder:            "hub-alice",
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EF",
				ClientKind:        "cli",
				ClaimKind:         "timed",
				AcquiredAt:        hubNow.Add(-time.Minute),
				ExpiresAt:         &expiresAt,
				Revision:          1,
				UpdatedAt:         hubNow,
			},
			HubNow: hubNow,
		}))
	}))
	t.Cleanup(hub.Close)
	enableSpokeClaimsTo(t, env, pid, hub.URL, 42)

	out := runCLI(t, env, dir, "show", created.ShortID)

	lines := leaseLines(out)
	require.Len(t, lines, 1)
	assert.Equal(t, "lease: hub-alice from instance 01HZNQ7VFPK1XGD8R5MABCD4EF (timed, 29m left)", lines[0])
}

func TestShow_JSONIncludesClaimFields(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "json claim issue")
	runCLIAs(t, env, dir, "alice", "federation", "lease", "acquire", ref)
	enqueueCLIPendingClaim(t, env.DB, pid, ref, "pending", time.Now().UTC())

	out := runCLI(t, env, dir, "--json", "show", ref)

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	assert.Contains(t, body, "lease")
	assert.Contains(t, body, "pending_leases")
	assert.Contains(t, body, "lease_hub_now")
}

func TestShow_ClaimViolationsRenderForFederatedIssueAndJSONCount(t *testing.T) {
	env, dir, pid, ref := setupFederatedHubIssue(t, "violated issue")
	ctx := context.Background()
	issue, err := env.DB.IssueByShortID(ctx, pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: pid,
		IssueRef:  ref,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: cliViolationSpokeUID,
			Holder:            "holder",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	for i := int64(0); i < 4; i++ {
		ingestCLIClaimViolation(t, env, pid, issue, "bob", "issue.updated", 20+i)
	}

	out := runCLI(t, env, dir, "show", ref)

	lines := claimViolationLines(out)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "lease violation: ")
	assert.Contains(t, lines[0], " issue.updated by bob from instance "+cliViolationSpokeUID+" ")
	assert.Contains(t, lines[0], "(uncovered_work)")
	assert.NotContains(t, out, "lease violations: 4")

	jsonOut := runCLI(t, env, dir, "--json", "show", ref)
	var body struct {
		ClaimViolationCount int `json:"lease_violation_count"`
		ClaimViolations     []struct {
			OffendingEventType        string `json:"offending_event_type"`
			OffendingOriginInstanceID string `json:"offending_origin_instance_uid"`
			Actor                     string `json:"actor"`
			Reason                    string `json:"reason"`
		} `json:"lease_violations"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &body))
	assert.Equal(t, 4, body.ClaimViolationCount)
	require.Len(t, body.ClaimViolations, 3)
	assert.Equal(t, "issue.updated", body.ClaimViolations[0].OffendingEventType)
	assert.Equal(t, cliViolationSpokeUID, body.ClaimViolations[0].OffendingOriginInstanceID)
	assert.Equal(t, "bob", body.ClaimViolations[0].Actor)
	assert.Equal(t, "uncovered_work", body.ClaimViolations[0].Reason)
}

// TestShow_LinkLabelInvertsOnToSide verifies that when show runs against
// the link's "to" side, the rendered LABEL inverts to read from the
// viewer's perspective: the parent slot's "to" end is the parent of
// the "from" end, so from the parent's POV (parent of child), the link
// reads "child: <child_short_id>".
func TestShow_LinkLabelInvertsOnToSide(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "parent")
	child := createIssue(t, env, pid, "child")
	// child → parent stores (from=child, to=parent). Showing parent puts
	// us on the to side.
	createLinkViaHTTP(t, env, pid, child, "parent", parent)

	out := runCLI(t, env, dir, "show", parent)
	assert.Contains(t, out, "child: "+child,
		"showing the parent issue must label the link as `child` from its POV")
}

func TestShow_AcceptsBareUIDAndQualified(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "uid target")
	_ = pid // pid not needed; we resolve via created.ShortID

	for _, ref := range []string{created.ShortID, "kata#" + created.ShortID, created.UID} {
		out := runCLI(t, env, dir, "show", ref)
		assert.Contains(t, out, "uid target", "ref %s", ref)
	}
}

// TestShow_LegacyNumberFails pins that bare numeric refs no longer resolve.
// The ResolveRef helper rejects them up-front with a guidance message.
func TestShow_LegacyNumberFails(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_, err := runCLICapture(t, env, dir, "show", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy issue number")
}

// TestShow_BareRefHonorsProjectFlagOutsideWorkspace pins that --project
// is consulted by ResolveRef as the fallback project name for bare
// refs when the workspace has no .kata.toml binding. Earlier the code
// passed only workspaceProjectName(start) so --project was ignored and
// the user got "no project bound to this workspace" even though they
// had explicitly named one.
func TestShow_BareRefHonorsProjectFlagOutsideWorkspace(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "ref outside workspace")

	// Use a fresh temp dir with no .kata.toml so workspaceProjectName
	// returns "". The --project flag must supply the project binding
	// ResolveRef needs to resolve the bare short_id.
	outside := t.TempDir()
	out := runCLI(t, env, outside, "--project", "kata", "show", created.ShortID)
	assert.Contains(t, out, "ref outside workspace")
}

func leaseLines(out string) []string {
	lines := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "lease: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func claimViolationLines(out string) []string {
	lines := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "lease violation: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func enqueueCLIPendingClaim(
	t *testing.T,
	store *sqlitestore.Store,
	projectID int64,
	ref string,
	holder string,
	at time.Time,
) db.PendingClaimRequest {
	t.Helper()
	pending, err := store.EnqueuePendingClaim(context.Background(), db.PendingClaimParams{
		ProjectID: projectID,
		IssueRef:  ref,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            holder,
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       at,
	})
	require.NoError(t, err)
	return pending
}
