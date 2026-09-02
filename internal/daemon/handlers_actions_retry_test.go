package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type closeRaceStore struct {
	db.Storage
	raceNext bool
}

type closeMoveRaceStore struct {
	db.Storage
	toProjectID int64
	moveNext    bool
}

func (s *closeMoveRaceStore) CloseIssueGuarded(
	ctx context.Context, params db.CloseIssueParams,
) (db.Issue, []db.Event, bool, error) {
	if s.moveNext {
		s.moveNext = false
		issue, err := s.IssueByID(ctx, params.IssueID)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: issue.ProjectID, ToProjectID: s.toProjectID,
			IfMatchRev: issue.Revision, Actor: "moving-agent",
		})
		if err != nil {
			return db.Issue{}, nil, false, err
		}
	}
	return s.Storage.CloseIssueGuarded(ctx, params)
}

func (s *closeRaceStore) CloseIssueGuarded(
	ctx context.Context, params db.CloseIssueParams,
) (db.Issue, []db.Event, bool, error) {
	if s.raceNext {
		s.raceNext = false
		_, _, _, err := s.Storage.CloseIssueGuarded(ctx, db.CloseIssueParams{
			IssueID: params.IssueID, Reason: "wontfix", Actor: "racing-agent",
		})
		if err != nil {
			return db.Issue{}, nil, false, err
		}
	}
	return s.Storage.CloseIssueGuarded(ctx, params)
}

type lostCloseResponseStore struct {
	db.Storage
	failNext            bool
	failEventLookupOnce bool
	failIssueReadOnce   bool
	committed           bool
	committedEvents     []db.Event
}

func (s *lostCloseResponseStore) CloseIssueGuarded(
	ctx context.Context, params db.CloseIssueParams,
) (db.Issue, []db.Event, bool, error) {
	issue, events, changed, err := s.Storage.CloseIssueGuarded(ctx, params)
	if err == nil && changed && s.failNext {
		s.failNext = false
		s.committed = true
		s.committedEvents = append([]db.Event(nil), events...)
		return issue, events, changed, errors.New("commit response lost")
	}
	return issue, events, changed, err
}

func (s *lostCloseResponseStore) EventsByUIDs(
	ctx context.Context, projectID int64, uids []string,
) ([]db.Event, error) {
	if s.committed && s.failEventLookupOnce {
		s.failEventLookupOnce = false
		return nil, errors.New("event lookup unavailable")
	}
	return s.Storage.EventsByUIDs(ctx, projectID, uids)
}

func (s *lostCloseResponseStore) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	if s.committed && s.failIssueReadOnce {
		s.failIssueReadOnce = false
		return db.Issue{}, errors.New("issue read unavailable")
	}
	return s.Storage.IssueByID(ctx, id)
}

func TestClose_RetryControlsRequireProtocolMarker(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"),
		map[string]string{"Idempotency-Key": "close-without-protocol"}, map[string]any{
			"actor":   "agent-one",
			"reason":  "wontfix",
			"message": "Reviewed the request and recorded why the work should stop here.",
		})
	assertAPIError(t, response.status, response.body,
		http.StatusBadRequest, "retry_protocol_required")
}

func TestClose_RetryProtocolMarkerIsCloseOnly(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}
	response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"),
		map[string]string{"Idempotency-Key": "close-with-protocol"}, body)
	requireOK(t, response)

	reopen := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/reopen"), nil,
		map[string]any{"actor": "agent-one", "retry_protocol": "close-v1"})
	assert.Equal(t, http.StatusBadRequest, reopen.status, string(reopen.body))
	assert.Contains(t, string(reopen.body), "retry_protocol")
}

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
		"actor":          "agent-one",
		"reason":         "done",
		"message":        "Implemented the requested behavior and ran the focused tests.",
		"retry_protocol": "close-v1",
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
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
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

func TestClose_IdempotencyReplaysReceiptAfterMove(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	path := issueURLRef(projectID, issue.ShortID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-request-then-move"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var firstOut api.MutationResponse
	require.NoError(t, json.Unmarshal(first.body, &firstOut.Body))
	require.NotNil(t, firstOut.Body.Event)

	target, err := h.DB().CreateProject(t.Context(), "target")
	require.NoError(t, err)
	moved, err := h.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issueID, FromProjectID: projectID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)

	retry := postWithHeader(t, ts, path, headers, body)
	requireOK(t, retry)
	var retryOut api.MutationResponse
	require.NoError(t, json.Unmarshal(retry.body, &retryOut.Body))
	assert.False(t, retryOut.Body.Changed)
	assert.True(t, retryOut.Body.Reused)
	require.NotNil(t, retryOut.Body.OriginalEvent)
	assert.Equal(t, firstOut.Body.Event.UID, retryOut.Body.OriginalEvent.UID)
	assert.Equal(t, moved.Issue.ProjectID, retryOut.Body.Issue.ProjectID)
}

func TestClose_RejectsKeyWhenAnotherCloseWinsBeforeWrite(t *testing.T) {
	database := openTestDB(t)
	project, err := database.db.CreateProject(t.Context(), "kata")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "race", Author: "agent-one",
	})
	require.NoError(t, err)
	store := &closeRaceStore{Storage: database.db, raceNext: true}
	ts := startTestServer(t, daemon.ServerConfig{DB: store, StartedAt: database.now})
	path := issueURLRef(project.ID, issue.ShortID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-race-1"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}

	first := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, first.status, first.body, http.StatusConflict, "issue_already_closed")

	_, _, changed, err := database.db.ReopenIssue(t.Context(), issue.ID, "coordinator")
	require.NoError(t, err)
	require.True(t, changed)

	retry := postWithHeader(t, ts, path, headers, body)
	requireOK(t, retry)
	var retryOut api.MutationResponse
	require.NoError(t, json.Unmarshal(retry.body, &retryOut.Body))
	assert.True(t, retryOut.Body.Changed)
	assert.False(t, retryOut.Body.Reused)
}

func TestClose_RejectsMoveThatWinsBeforeGuardedWrite(t *testing.T) {
	database := openTestDB(t)
	source, err := database.db.CreateProject(t.Context(), "source")
	require.NoError(t, err)
	target, err := database.db.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: source.ID, Title: "move race", Author: "agent-one",
	})
	require.NoError(t, err)
	store := &closeMoveRaceStore{Storage: database.db, toProjectID: target.ID, moveNext: true}
	ts := startTestServer(t, daemon.ServerConfig{DB: store, StartedAt: database.now})
	response := postWithHeader(t, ts,
		issueURLRef(source.ID, issue.ShortID, "actions/close"),
		map[string]string{"Idempotency-Key": "close-move-race-1"}, map[string]any{
			"actor":          "agent-one",
			"reason":         "wontfix",
			"message":        "Reviewed the request and recorded why the work should stop here.",
			"retry_protocol": "close-v1",
		})
	assertAPIError(t, response.status, response.body, http.StatusConflict, "issue_moved")

	current, err := database.db.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, target.ID, current.ProjectID)
	assert.Equal(t, "open", current.Status)
}

func TestClose_RecoversCommittedReceiptAndPublishesEventsOnce(t *testing.T) {
	database := openTestDB(t)
	project, issue := createClaimHubIssueInDB(t, database.db)
	_, err := database.db.AcquireClaim(t.Context(), db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: database.db.InstanceUID(), Holder: "agent-one", ClientKind: "cli",
		},
		ClaimKind: "hard", Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	store := &lostCloseResponseStore{Storage: database.db, failNext: true}
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer subscription.Unsub()
	ts := startTestServer(t, daemon.ServerConfig{
		DB: store, StartedAt: database.now, Hooks: sink, Broadcaster: broadcaster,
	})
	path := issueURLRef(project.ID, issue.ShortID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-lost-response-1"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var firstOut api.MutationResponse
	require.NoError(t, json.Unmarshal(first.body, &firstOut.Body))
	assert.False(t, firstOut.Body.Changed)
	assert.True(t, firstOut.Body.Reused)
	require.Len(t, store.committedEvents, 2)
	assert.Equal(t, []string{"issue.closed", "claim.released"}, []string{
		store.committedEvents[0].Type, store.committedEvents[1].Type,
	})
	assert.Equal(t, store.committedEvents, sink.snapshot())
	assert.Equal(t, []int64{store.committedEvents[0].ID, store.committedEvents[1].ID},
		drainBroadcastIDs(t, subscription.Ch, 50*time.Millisecond))

	second := postWithHeader(t, ts, path, headers, body)
	requireOK(t, second)
	assert.Equal(t, store.committedEvents, sink.snapshot(), "an exact retry must not publish twice")
	assert.Empty(t, drainBroadcastIDs(t, subscription.Ch, 50*time.Millisecond))
}

func TestClose_RetryRecoversUndeliveredCommitWithoutDuplicateEvents(t *testing.T) {
	database := openTestDB(t)
	project, issue := createClaimHubIssueInDB(t, database.db)
	_, err := database.db.AcquireClaim(t.Context(), db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: database.db.InstanceUID(), Holder: "agent-one", ClientKind: "cli",
		},
		ClaimKind: "hard", Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	store := &lostCloseResponseStore{
		Storage: database.db, failNext: true,
		failEventLookupOnce: true, failIssueReadOnce: true,
	}
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer subscription.Unsub()
	ts := startTestServer(t, daemon.ServerConfig{
		DB: store, StartedAt: database.now, Hooks: sink, Broadcaster: broadcaster,
	})
	path := issueURLRef(project.ID, issue.ShortID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-delivery-retry-1"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}

	first := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, first.status, first.body, http.StatusInternalServerError, "internal")
	require.Len(t, store.committedEvents, 2)
	assert.Empty(t, sink.snapshot())
	assert.Empty(t, drainBroadcastIDs(t, subscription.Ch, 50*time.Millisecond))

	second := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, second.status, second.body, http.StatusInternalServerError, "internal")
	assert.Equal(t, store.committedEvents, sink.snapshot())
	assert.Equal(t, []int64{store.committedEvents[0].ID, store.committedEvents[1].ID},
		drainBroadcastIDs(t, subscription.Ch, 50*time.Millisecond))

	third := postWithHeader(t, ts, path, headers, body)
	requireOK(t, third)
	assert.Equal(t, store.committedEvents, sink.snapshot())
	assert.Empty(t, drainBroadcastIDs(t, subscription.Ch, 50*time.Millisecond))
}

func TestClose_IdempotencyRejectsDifferentRequest(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(projectID, issueID, "actions/close")
	headers := map[string]string{"Idempotency-Key": "close-request-1"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}
	requireOK(t, postWithHeader(t, ts, path, headers, body))

	body["message"] = "A different explanation must not reuse the original close receipt."
	retry := postWithHeader(t, ts, path, headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusConflict, "idempotency_mismatch")
}

func TestClose_IdempotencyRejectsDifferentIssueWithSameBody(t *testing.T) {
	h, ts, projectID, firstID := bootstrapProjectWithIssue(t)
	first, err := h.DB().IssueByID(t.Context(), firstID)
	require.NoError(t, err)
	second, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: projectID, Title: "second issue", Author: "agent-one",
	})
	require.NoError(t, err)
	headers := map[string]string{"Idempotency-Key": "close-request-1"}
	body := map[string]any{
		"actor":          "agent-one",
		"reason":         "wontfix",
		"message":        "Reviewed the request and recorded why the work should stop here.",
		"retry_protocol": "close-v1",
	}
	requireOK(t, postWithHeader(t, ts,
		issueURLRef(projectID, first.ShortID, "actions/close"), headers, body))

	retry := postWithHeader(t, ts,
		issueURLRef(projectID, second.ShortID, "actions/close"), headers, body)
	assertAPIError(t, retry.status, retry.body, http.StatusConflict, "idempotency_mismatch")
	current, err := h.DB().IssueByID(t.Context(), second.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", current.Status)
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
		"actor":          "agent-one",
		"reason":         "done",
		"message":        "Implemented the requested behavior and ran the focused tests.",
		"retry_protocol": "close-v1",
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

	body["retry_protocol"] = "close-v1"
	response := postWithHeader(t, ts, path,
		map[string]string{"If-Match": `"rev-1"`}, body)
	assertAPIError(t, response.status, response.body,
		http.StatusPreconditionFailed, "revision_conflict")
}
