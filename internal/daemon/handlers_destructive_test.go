package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// confirmHeader builds the X-Kata-Confirm value for an issue. After Task 11
// the daemon expects "<verb> <project>#<short_id>" rather than the old
// "<verb> #N" form.
func confirmHeader(t *testing.T, h *httptestServerHandle, projectID, issueID int64, verb string) string {
	t.Helper()
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	project, err := h.DB().ProjectByID(t.Context(), projectID)
	require.NoError(t, err)
	return verb + " " + project.Name + "#" + issue.ShortID
}

func TestDelete_RequiresConfirmHeader(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		nil, // no header
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 412, "confirm_required")
}

func TestDelete_RejectsWrongConfirmValue(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		map[string]string{"X-Kata-Confirm": "DELETE kata#zzzz"}, // wrong short_id
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 412, "confirm_mismatch")
}

func TestDelete_AcceptsCorrectConfirmAndSoftDeletes(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "DELETE")},
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp.status, string(resp.body))
	assert.Contains(t, string(resp.body), `"changed":true`)
	assert.Contains(t, string(resp.body), `"issue.soft_deleted"`)

	// show without include_deleted now 404s.
	respShow, bs := getStatusBody(t, ts, issueURL(pid, num, ""))
	require.Equal(t, 404, respShow.StatusCode, string(bs))
}

func TestDelete_AlreadyDeletedIsNoOp(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)
	hdr := confirmHeader(t, h, pid, num, "DELETE")
	requireOK(t, postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		map[string]string{"X-Kata-Confirm": hdr},
		map[string]any{"actor": "agent"}))

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		map[string]string{"X-Kata-Confirm": hdr},
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp.status, string(resp.body))
	assert.Contains(t, string(resp.body), `"changed":false`)
	assert.Contains(t, string(resp.body), `"event":null`)
}

func TestRestore_ClearsDeletedAt(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)
	requireOK(t, postWithHeader(t, ts, issueURL(pid, num, "actions/delete"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "DELETE")},
		map[string]any{"actor": "agent"}))

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/restore"),
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"changed":true`)
	assert.Contains(t, string(bs), `"issue.restored"`)

	// show without include_deleted works again.
	respShow, bsShow := getStatusBody(t, ts, issueURL(pid, num, ""))
	require.Equal(t, 200, respShow.StatusCode, string(bsShow))
}

func TestPurge_RequiresConfirmHeaderAndRemovesAllRows(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), num)
	require.NoError(t, err)
	project, err := h.DB().ProjectByID(t.Context(), pid)
	require.NoError(t, err)

	// Missing header → 412.
	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		nil, map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 412, "confirm_required")

	// Wrong header → 412 confirm_mismatch.
	resp = postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		map[string]string{"X-Kata-Confirm": "DELETE kata#zzzz"},
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 412, "confirm_mismatch")

	// Correct header → 200 with purge_log.
	resp = postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "PURGE")},
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp.status, string(resp.body))
	assert.Contains(t, string(resp.body), `"purge_log"`)
	assert.Contains(t, string(resp.body), `"purged_issue_id"`)
	assert.Contains(t, string(resp.body), `"issue_uid":"`+issue.UID+`"`)
	assert.Contains(t, string(resp.body), `"project_uid":"`+project.UID+`"`)

	// Subsequent show 404s — issue is gone.
	respShow, _ := getStatusBody(t, ts, issueURL(pid, num, "")+"?include_deleted=true")
	assert.Equal(t, 404, respShow.StatusCode)
}

func TestPurge_FederatedSpokeRequiresHubAdmin(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)
	project, err := h.DB().ProjectByID(t.Context(), pid)
	require.NoError(t, err)
	_, err = h.DB().UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "PURGE")},
		map[string]any{"actor": "agent"})

	assertAPIError(t, resp.status, resp.body, 409, "federated_admin_required")
}

func TestPurge_FederatedHubRequiresIssueClaim(t *testing.T) {
	h, ts, pid, num := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), num)
	require.NoError(t, err)
	_, err = h.DB().EnableProjectFederation(t.Context(), pid, "tester")
	require.NoError(t, err)
	_, err = h.DB().AcquireClaim(t.Context(), db.AcquireClaimParams{
		ProjectID: pid,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: h.DB().InstanceUID(),
			Holder:            "alice",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	resp := postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "PURGE")},
		map[string]any{"actor": "bob"})
	assertAPIError(t, resp.status, resp.body, 409, "claim_denied")

	resp = postWithHeader(t, ts, issueURL(pid, num, "actions/purge"),
		map[string]string{"X-Kata-Confirm": confirmHeader(t, h, pid, num, "PURGE")},
		map[string]any{"actor": "alice"})
	require.Equal(t, 200, resp.status, string(resp.body))
	assert.Contains(t, string(resp.body), `"purge_log"`)
}

func TestPurge_FederatedHubUsesResolvedIdentityActorForClaimGate(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	project, issue := createClaimHubIssue(t, env)
	_, _, err := env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "alice",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "actions/purge"),
		map[string]any{"actor": ""}, map[string]string{
			"Authorization":  "Bearer alice-token",
			"X-Kata-Confirm": "PURGE " + project.Name + "#" + issue.ShortID,
		})

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"purge_log"`)
}

// TestDelete_UnknownIssueIs404 covers the handler-level translation of
// db.ErrNotFound into the 404 issue_not_found wire envelope. The DB layer
// has its own no-op tests; this pins the handler edge.
func TestDelete_UnknownIssueIs404(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	resp := postWithHeader(t, ts, issueURL(pid, 9999, "actions/delete"),
		map[string]string{"X-Kata-Confirm": "DELETE #9999"},
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 404, "issue_not_found")
}

// TestRestore_UnknownIssueIs404 mirrors TestDelete_UnknownIssueIs404 for the
// restore route (no confirm header gate, but the lookup still 404s).
func TestRestore_UnknownIssueIs404(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	resp, bs := postJSON(t, ts, issueURL(pid, 9999, "actions/restore"),
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.StatusCode, bs, 404, "issue_not_found")
}

// TestPurge_UnknownIssueIs404 mirrors the delete/restore 404 pin for purge.
func TestPurge_UnknownIssueIs404(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	resp := postWithHeader(t, ts, issueURL(pid, 9999, "actions/purge"),
		map[string]string{"X-Kata-Confirm": "PURGE #9999"},
		map[string]any{"actor": "agent"})
	assertAPIError(t, resp.status, resp.body, 404, "issue_not_found")
}
