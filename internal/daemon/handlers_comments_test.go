package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type commentProjectHostAccess struct {
	deniedProjectID int64
}

func (a commentProjectHostAccess) Authorize(
	_ context.Context,
	request daemon.HostAccessRequest,
) (daemon.HostAccessDecision, error) {
	if slices.Contains(request.Operation.ProjectIDs, a.deniedProjectID) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{}, nil
}

func TestCommentEndpoint_AppendsAndEmitsEvent(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "agent", "body": "first comment"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"body":"first comment"`)
	assert.Contains(t, string(bs), `"type":"issue.commented"`)
}

func TestCommentEndpoint_IdempotencyReusesCommittedComment(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(pid, issueID, "comments")
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-1"}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var created struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(first.body, &created))
	require.NotEmpty(t, created.Comment.UID)

	second := postWithHeader(t, ts, path, headers, body)
	requireOK(t, second)
	var reused struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
		Event   *struct{} `json:"event"`
		Changed bool      `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(second.body, &reused))
	assert.Equal(t, created.Comment.UID, reused.Comment.UID)
	assert.Nil(t, reused.Event)
	assert.False(t, reused.Changed)

	comments, err := h.DB().CommentsByIssue(context.Background(), issueID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestCommentEndpoint_IdempotencyReplaysCommittedCommentAfterMove(t *testing.T) {
	h, ts, sourceProjectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-then-move"}

	first := postWithHeader(t, ts, issueURLRef(sourceProjectID, issue.UID, "comments"), headers, body)
	requireOK(t, first)
	target, err := h.DB().CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, err = h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	_, err = h.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: sourceProjectID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)

	retry := postWithHeader(t, ts, issueURLRef(target.ID, issue.UID, "comments"), headers, body)
	requireOK(t, retry)
	var reused struct {
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(retry.body, &reused))
	assert.False(t, reused.Changed)
	comments, err := h.DB().CommentsByIssue(t.Context(), issueID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestCommentEndpoint_IdempotencyReauthorizesMovedIssue(t *testing.T) {
	dbh, initialServer, sourceProjectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := dbh.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-before-move"}
	path := issueURLRef(sourceProjectID, issue.UID, "comments")
	requireOK(t, postWithHeader(t, initialServer, path, headers, body))

	target, err := dbh.DB().CreateProject(t.Context(), "denied-target")
	require.NoError(t, err)
	issue, err = dbh.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	_, err = dbh.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: sourceProjectID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)

	server := daemon.NewServer(daemon.ServerConfig{
		DB: dbh.DB(), StartedAt: time.Now(),
		HostAccess: commentProjectHostAccess{deniedProjectID: target.ID},
	})
	hostServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := daemon.WithPrincipal(request.Context(), daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "agent",
		})
		server.Handler().ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(hostServer.Close)

	retry := postWithHeader(t, hostServer, path, headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusNotFound, "not_found")
}

func TestCommentEndpoint_IdempotencyRejectsDifferentBody(t *testing.T) {
	_, ts, pid, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(pid, issueID, "comments")
	headers := map[string]string{"Idempotency-Key": "comment-request-1"}

	first := postWithHeader(t, ts, path, headers,
		map[string]any{"actor": "agent", "body": "first comment"})
	requireOK(t, first)
	second := postWithHeader(t, ts, path, headers,
		map[string]any{"actor": "agent", "body": "different comment"})

	assertAPIError(t, second.status, second.body, 409, "idempotency_mismatch")
}

func TestCommentEndpoint_EditsCommentAndEmitsEvent(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "agent", "body": "token=leaked"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	var created struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	require.NotEmpty(t, created.Comment.UID)

	editResp, editBS := patchJSON(t, ts, issueURL(pid, num, "comments/"+created.Comment.UID),
		map[string]any{"actor": "redactor", "body": "[redacted]"})
	require.Equal(t, 200, editResp.StatusCode, string(editBS))
	assert.Contains(t, string(editBS), `"body":"[redacted]"`)
	assert.Contains(t, string(editBS), `"type":"issue.comment_edited"`)
	assert.NotContains(t, string(editBS), "token=leaked")
}

func TestCommentEndpoint_RejectsEditingExternallyOwnedComment(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	observedAt := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	binding, _, err := h.DB().CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: pid, IssueID: issueID, ConnectorInstance: "example-connector",
		ExternalRootKey: "external-root", ExternalAccountKey: "external-account",
		Actor: "connector:example-connector", ReceiveCommentsAfter: observedAt,
	})
	require.NoError(t, err)
	claimed, acquired, err := h.DB().ClaimExternalRootBinding(
		t.Context(), binding.ID, "projection-claim", observedAt, observedAt.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	comment, _, _, err := h.DB().UpsertExternalCommentProjection(t.Context(), db.ExternalCommentProjectionParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken,
		ExternalID: "external-comment", ExternalRevision: "revision-1",
		Body: "Provider-owned comment", ExternalActorID: "external-actor",
		ExternalActorName: "External author", ExternalCreatedAt: observedAt,
		ExternalUpdatedAt: observedAt, IntegrationActor: "connector:example-connector",
	})
	require.NoError(t, err)

	resp, body := patchJSON(t, ts, issueURL(pid, issueID, "comments/"+comment.UID),
		map[string]any{"actor": "redactor", "body": "local replacement"})

	assertAPIError(t, resp.StatusCode, body, 409, "external_comment_content_owned")
}

func TestActionsClose_ReopenRoundtrip(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{
			"actor":   "agent",
			"reason":  "wontfix",
			"message": "Decided not to fix this; out of scope for this milestone and not aligned with roadmap.",
		})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"status":"closed"`)
	assert.Contains(t, string(bs), `"closed_reason":"wontfix"`)

	resp2, bs2 := postJSON(t, ts, issueURL(pid, num, "actions/reopen"),
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))
	assert.Contains(t, string(bs2), `"status":"open"`)
}

func TestActionsClose_RejectsUnsupportedReason(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{"actor": "agent", "reason": "obsolete"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestActionsClose_AlreadyClosedIsNoOpEnvelope(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)
	body := map[string]any{
		"actor":   "agent",
		"reason":  "wontfix",
		"message": "Decided not to fix this; out of scope for this milestone and not aligned with roadmap.",
	}
	_, _ = postJSON(t, ts, issueURL(pid, num, "actions/close"), body)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"), body)
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"changed":false`)
	assert.Contains(t, string(bs), `"event":null`)
}

func TestCreateComment_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "   ", "body": "hi"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestCloseIssue_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{"actor": "   "})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestReopenIssue_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/reopen"),
		map[string]any{"actor": "   "})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestCommentEndpoint_IdempotencyKeyIsScopedToIssue(t *testing.T) {
	h, ts, pid, firstIssueID := bootstrapProjectWithIssue(t)
	second, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: pid, Title: "second issue", Author: "agent",
	})
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "same body"}
	headers := map[string]string{"Idempotency-Key": "shared-key"}

	first := postWithHeader(t, ts, issueURL(pid, firstIssueID, "comments"), headers, body)
	requireOK(t, first)
	var firstOut struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(first.body, &firstOut))

	other := postWithHeader(t, ts, issueURLRef(pid, second.ShortID, "comments"), headers, body)
	requireOK(t, other)
	var otherOut struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(other.body, &otherOut))
	assert.True(t, otherOut.Changed, "a key used on another issue must not replay the first issue's comment")
	assert.NotEqual(t, firstOut.Comment.UID, otherOut.Comment.UID)
	comments, err := h.DB().CommentsByIssue(t.Context(), second.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestCommentEndpoint_IdempotencyReplayRequiresRelatedProject(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-unrelated-route"}
	requireOK(t, postWithHeader(t, ts, issueURLRef(pid, issue.UID, "comments"), headers, body))

	unrelated, err := h.DB().CreateProject(t.Context(), "unrelated-project")
	require.NoError(t, err)
	retry := postWithHeader(t, ts, issueURLRef(unrelated.ID, issue.UID, "comments"), headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusNotFound, "issue_not_found")
}

func TestCommentEndpoint_IdempotencyReplayRejectsArchivedCurrentProject(t *testing.T) {
	h, ts, sourceProjectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-then-archive"}
	path := issueURLRef(sourceProjectID, issue.UID, "comments")
	requireOK(t, postWithHeader(t, ts, path, headers, body))

	target, err := h.DB().CreateProject(t.Context(), "archived-target")
	require.NoError(t, err)
	issue, err = h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	_, err = h.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: sourceProjectID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)
	archiveProject(t, h, target.ID, true)

	retry := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusNotFound, "project_not_found")
}

func TestCommentEndpoint_IdempotencyReplaysShortIDRetryAfterMove(t *testing.T) {
	h, ts, sourceProjectID, issueID := bootstrapProjectWithIssue(t)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "short-id-comment-then-move"}
	path := issueURL(sourceProjectID, issueID, "comments")
	requireOK(t, postWithHeader(t, ts, path, headers, body))

	target, err := h.DB().CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	_, err = h.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: sourceProjectID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)

	retry := postWithHeader(t, ts, path, headers, body)
	requireOK(t, retry)
	var reused struct {
		Changed bool `json:"changed"`
		Issue   struct {
			ProjectID int64 `json:"project_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(retry.body, &reused))
	assert.False(t, reused.Changed)
	assert.Equal(t, target.ID, reused.Issue.ProjectID)
	comments, err := h.DB().CommentsByIssue(t.Context(), issueID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestCommentEndpoint_IdempotencyReplayRejectsSoftDeletedIssue(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-then-delete"}
	path := issueURLRef(pid, issue.UID, "comments")
	requireOK(t, postWithHeader(t, ts, path, headers, body))

	_, _, _, err = h.DB().SoftDeleteIssue(t.Context(), issueID, "agent")
	require.NoError(t, err)

	retry := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusNotFound, "issue_not_found")
}
