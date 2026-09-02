package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestClose_IdempotencyReplaysCommittedReceipt(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(context.Background(), issueID)
	require.NoError(t, err)
	path := issueURLRef(projectID, issue.ShortID, "actions/close")
	headers := map[string]string{
		"Idempotency-Key": "close-request-1",
		"If-Match":        `"rev-1"`,
	}
	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "done",
		"message": "Implemented the requested behavior and ran the focused tests.",
		"evidence": []map[string]any{{
			"type": "test", "command": "go test ./internal/daemon",
		}},
	}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var firstOut api.MutationResponse
	require.NoError(t, json.Unmarshal(first.body, &firstOut.Body))
	require.True(t, firstOut.Body.Changed)
	require.NotNil(t, firstOut.Body.Event)
	assert.False(t, firstOut.Body.Reused)

	second := postWithHeader(t, ts, path, headers, body)
	requireOK(t, second)
	var secondOut api.MutationResponse
	require.NoError(t, json.Unmarshal(second.body, &secondOut.Body))
	assert.False(t, secondOut.Body.Changed)
	assert.True(t, secondOut.Body.Reused)
	assert.Nil(t, secondOut.Body.Event)
	require.NotNil(t, secondOut.Body.OriginalEvent)
	assert.Equal(t, firstOut.Body.Event.UID, secondOut.Body.OriginalEvent.UID)

	events, err := h.DB().EventsAfter(context.Background(), db.EventsAfterParams{
		ProjectID: projectID,
		Limit:     100,
	})
	require.NoError(t, err)
	closed := 0
	for _, event := range events {
		if event.Type == "issue.closed" && event.IssueID != nil && *event.IssueID == issueID {
			closed++
		}
	}
	assert.Equal(t, 1, closed)
}

func TestClose_IdempotencyReplaysReceiptAfterSoftDelete(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(context.Background(), issueID)
	require.NoError(t, err)
	path := issueURLRef(projectID, issue.ShortID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-request-then-delete"}
	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "wontfix",
		"message": "Reviewed the request and recorded why the work should stop here.",
	}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var firstOut api.MutationResponse
	require.NoError(t, json.Unmarshal(first.body, &firstOut.Body))
	require.NotNil(t, firstOut.Body.Event)

	_, _, changed, err := h.DB().SoftDeleteIssue(context.Background(), issueID, "agent-one")
	require.NoError(t, err)
	require.True(t, changed)

	retry := postWithHeader(t, ts, path, headers, body)
	requireOK(t, retry)
	var retryOut api.MutationResponse
	require.NoError(t, json.Unmarshal(retry.body, &retryOut.Body))
	assert.False(t, retryOut.Body.Changed)
	assert.True(t, retryOut.Body.Reused)
	require.NotNil(t, retryOut.Body.OriginalEvent)
	assert.Equal(t, firstOut.Body.Event.UID, retryOut.Body.OriginalEvent.UID)
	require.NotNil(t, retryOut.Body.Issue.DeletedAt)
}

func TestClose_IdempotencyRejectsDifferentRequest(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(projectID, issueID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-request-1"}
	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "wontfix",
		"message": "Reviewed the request and recorded why the work should stop here.",
	}
	requireOK(t, postWithHeader(t, ts, path, headers, body))

	body["message"] = "A different explanation must not reuse the original close receipt."
	retry := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusConflict, "idempotency_mismatch")
}

func TestClose_IfMatchRejectsStaleRevision(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	_, err := h.DB().PatchIssueMetadata(context.Background(), db.PatchIssueMetadataIn{
		IssueID: issueID,
		Actor:   "coordinator",
		Patch: map[string]json.RawMessage{
			"work.state": json.RawMessage(`"ready"`),
		},
	})
	require.NoError(t, err)

	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "done",
		"message": "Implemented the requested behavior and ran the focused tests.",
		"evidence": []map[string]any{{
			"type": "test", "command": "go test ./internal/daemon",
		}},
	}
	response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"),
		map[string]string{"If-Match": `"rev-1"`}, body)
	assertAPIError(t, response.status, response.body,
		http.StatusPreconditionFailed, "revision_conflict")

	issue, err := h.DB().IssueByID(context.Background(), issueID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
}

func TestClose_PresentEmptyIfMatchRejectsRequest(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "wontfix",
		"message": "Reviewed the request and recorded why the work should stop here.",
	}
	response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"),
		map[string]string{"If-Match": ""}, body)
	assertAPIError(t, response.status, response.body,
		http.StatusBadRequest, "validation")

	issue, err := h.DB().IssueByID(context.Background(), issueID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
}

func TestClose_IfMatchRejectsStaleRevisionAfterAnotherClose(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	_, err := h.DB().PatchIssueMetadata(context.Background(), db.PatchIssueMetadataIn{
		IssueID: issueID,
		Actor:   "coordinator",
		Patch: map[string]json.RawMessage{
			"work.state": json.RawMessage(`"ready"`),
		},
	})
	require.NoError(t, err)
	path := issueURL(projectID, issueID, "actions/close")
	body := map[string]any{
		"actor":   "agent-one",
		"reason":  "wontfix",
		"message": "Reviewed the request and recorded why the work should stop here.",
	}
	requireOK(t, postWithHeader(t, ts, path, nil, body))

	response := postWithHeader(t, ts, path,
		map[string]string{"If-Match": `"rev-1"`}, body)
	assertAPIError(t, response.status, response.body,
		http.StatusPreconditionFailed, "revision_conflict")
}
