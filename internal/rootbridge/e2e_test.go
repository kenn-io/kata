package rootbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/connector/fakeconnector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/pkg/connector"
)

func TestExternalRootBridgeEndToEnd(t *testing.T) {
	executable := buildFakeConnector(t)
	backends := []e2eBackend{{name: "sqlite", open: openSQLiteE2E}}
	if dsn := strings.TrimSpace(os.Getenv("KATA_TEST_POSTGRES_DSN")); dsn != "" {
		backends = append(backends, e2eBackend{
			name: "postgres",
			open: func(t *testing.T) *e2eDatabase { return openPostgresE2E(t, dsn) },
		})
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Run("restart inbound quiet completion and reopen", func(t *testing.T) {
				testE2ERestartInboundCompletion(t, executable, backend)
			})
			t.Run("pending publication recovery actions", func(t *testing.T) {
				testE2EPendingPublication(t, executable, backend)
			})
			t.Run("planning field conflict and completion isolation", func(t *testing.T) {
				testE2EPlanningFields(t, executable, backend)
			})
			t.Run("planning field wire shapes", func(t *testing.T) {
				testE2EPlanningFieldShapes(t, executable, backend)
			})
			t.Run("restore requires identity reconfirmation", func(t *testing.T) {
				testE2ERestorePause(t, executable, backend)
			})
			t.Run("process and protocol failure isolation", func(t *testing.T) {
				testE2EFailureIsolation(t, executable, backend)
			})
		})
	}
}

func TestRecordedExternalSurfaceRejectsLocalIdentityValues(t *testing.T) {
	for _, test := range []struct {
		localIdentity string
		long          bool
	}{
		{localIdentity: "01ARZ3NDEKTSV4RRFFQ69G5FAV", long: true},
		{localIdentity: "root-short"},
		{localIdentity: "01ARZ3NDEKTSV4RRFFQ69G5FAW", long: true},
		{localIdentity: "child-short"},
	} {
		current := fakeconnector.State{Calls: []fakeconnector.Call{{
			Method: "publish_comment",
			Params: json.RawMessage(`{"root_key":"root-example","body":` + strconv.Quote(test.localIdentity) + `}`),
		}}}
		if test.long {
			require.Error(t, fakeconnector.AuditExternalSurface(current, "root-example", []string{test.localIdentity}, nil))
		} else {
			require.Error(t, fakeconnector.AuditExternalSurface(current, "root-example", nil, []string{test.localIdentity}))
		}
	}
}

func TestRecordedExternalSurfaceRejectsEmbeddedLongUIDsOnly(t *testing.T) {
	rootUID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	childUID := "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	for _, uid := range []string{rootUID, childUID} {
		current := fakeconnector.State{Calls: []fakeconnector.Call{{
			Method: "publish_comment",
			Params: json.RawMessage(`{"root_key":"root-example","body":` + strconv.Quote("prefix-"+uid+"-suffix") + `,"operation_id":"operation-example"}`),
		}}}
		require.Error(t, fakeconnector.AuditExternalSurface(current, "root-example", []string{uid}, nil))
	}

	for _, body := range []string{
		"prefix-root-short-suffix",
		"unrelated-01ARZ3NDEKTSV4RRFFQ69G5FAX-suffix",
	} {
		current := fakeconnector.State{Calls: []fakeconnector.Call{{
			Method: "publish_comment",
			Params: json.RawMessage(`{"root_key":"root-example","body":` + strconv.Quote(body) + `,"operation_id":"operation-example"}`),
		}}}
		require.NoError(t, fakeconnector.AuditExternalSurface(current, "root-example", []string{rootUID, childUID}, []string{"root-short"}))
	}
}

func TestRecordedExternalSurfaceRejectsForbiddenKataKeysInsideFieldValues(t *testing.T) {
	for _, key := range []string{
		"kata_uid", "kataUid", "kata-uid", "kata.uid",
		"kata_ref", "kataRef", "kata-ref", "kata.ref",
		"kata_project_id", "kataProjectId", "kata-project-id", "kata.project.id",
		"kata_binding_id", "kataBindingId", "kata-binding-id", "kata.binding.id",
		"kata_work_branch", "kataWorkBranch", "kata-work-branch", "kata.work.branch",
	} {
		params, err := json.Marshal(map[string]any{
			"root_key": "root-example",
			"fields": map[string]any{
				"kata_uid": map[string]any{"kind": "date", "value": "2026-08-20", key: "neutral-forbidden"},
			},
		})
		require.NoError(t, err)
		current := fakeconnector.State{Calls: []fakeconnector.Call{{Method: "write_fields", Params: params}}}
		require.Error(t, fakeconnector.AuditExternalSurface(current, "root-example", nil, nil), key)
	}
}

func testE2ERestartInboundCompletion(t *testing.T, executable string, backend e2eBackend) {
	h := newE2EHarness(t, executable, backend, false)
	child, _, err := h.database.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Child title", Body: "Child body", Author: "operator",
	})
	require.NoError(t, err)
	for _, uid := range []string{h.issue.UID, child.UID} {
		params, marshalErr := json.Marshal(map[string]any{
			"root_key": "root-example",
			"body":     "embedded-" + uid + "-identity",
		})
		require.NoError(t, marshalErr)
		probe := fakeconnector.State{Calls: []fakeconnector.Call{{Method: "publish_comment", Params: params}}}
		require.Error(t, fakeconnector.AuditExternalSurface(probe, "root-example", []string{uid}, nil))
	}
	_, err = h.database.store.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: h.issue.ID, Type: "parent", Author: "operator",
	})
	require.NoError(t, err)
	_, _, err = h.database.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: child.ID, Author: "operator", Body: "Child-only note",
	})
	require.NoError(t, err)
	_, err = h.database.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: child.ID, Actor: "operator", Patch: map[string]json.RawMessage{
			"work.attention": json.RawMessage(`"ok"`),
		},
	})
	require.NoError(t, err)
	_, _, _, err = h.database.store.CloseIssue(
		t.Context(), child.ID, "done", "verifier", "Verified child independently.",
		[]db.Evidence{{Type: "test", Command: "go test ./internal/rootbridge"}},
	)
	require.NoError(t, err)
	childBefore, err := h.database.store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	childCommentsBefore, err := h.database.store.CommentsByIssue(t.Context(), child.ID)
	require.NoError(t, err)

	issue, err := h.database.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "External title", issue.Title)
	assert.Equal(t, "External body", issue.Body)
	comments, err := h.database.store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Empty(t, comments, "pre-bind native comments must remain external")

	_, _, err = h.database.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "operator", Body: "Quiet local note",
	})
	require.NoError(t, err)
	externalComment := connector.Comment{
		ID: "comment-after-bind", Revision: "revision-comment-after-bind", Body: "Please verify the result",
		Author:    connector.Actor{ID: "actor-reviewer", DisplayName: "Reviewer"},
		CreatedAt: h.base.Add(time.Minute), UpdatedAt: h.base.Add(time.Minute),
	}
	h.mutateState(func(current *fakeconnector.State) {
		current.Roots[0].Comments = append(current.Roots[0].Comments, externalComment)
	})
	h.restart(t)

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, first.CommentsCreated)
	assert.Zero(t, second.CommentsCreated)
	comments, err = h.database.store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	var imported db.Comment
	for _, comment := range comments {
		if comment.Author == "connector:example" {
			imported = comment
		}
	}
	require.NotZero(t, imported.ID)
	assert.Equal(t, externalComment.Body, imported.Body)
	assert.Equal(t, externalComment.CreatedAt, imported.CreatedAt)

	event := requireEventType(t, first.Events, "issue.commented")
	var payload struct {
		Source db.ExternalProjectionSource `json:"source"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, "example", payload.Source.ConnectorInstance)
	assert.Equal(t, externalComment.ID, payload.Source.ExternalCommentID)
	assert.Equal(t, externalComment.Author.ID, payload.Source.ActorID)
	assert.Equal(t, externalComment.Author.DisplayName, payload.Source.ActorName)

	_, _, _, err = h.database.store.CloseIssue(
		t.Context(), issue.ID, "done", "verifier", "Verified completion.",
		[]db.Evidence{{Type: "test", Command: "go test ./internal/rootbridge"}},
	)
	require.NoError(t, err)
	closedRun, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Zero(t, closedRun.CompletionRequests)
	state := h.readState(t)
	assert.Equal(t, "complete", state.Roots[0].Root.State)
	assert.Len(t, state.Roots, 1, "bridge must not create child roots")
	assert.Equal(t, 0, mutationCount(state, "publish_comment"), "quiet mode must not publish comments")
	assert.Equal(t, 1, mutationCount(state, "complete_root"))

	h.mutateState(func(current *fakeconnector.State) {
		root := &current.Roots[0].Root
		root.State = "open"
		root.Revision = "revision-reopened"
		root.UpdatedAt = h.base.Add(2 * time.Minute)
		root.ObservedAt = root.UpdatedAt
		root.Actor = &connector.Actor{ID: "actor-coordinator", DisplayName: "Coordinator"}
	})
	reopened, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, reopened.ReopenRequests)
	issue, err = h.database.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "closed", issue.Status, "external lifecycle never reopens Kata")
	state = h.readState(t)
	assert.Equal(t, "complete", state.Roots[0].Root.State, "verified Kata close re-completes an externally reopened root")
	assert.Len(t, state.Roots, 1)
	assertExternalCallsContainNoKataOrChildIdentity(
		t, state,
		[]string{h.issue.UID, child.UID},
		[]string{h.issue.ShortID, child.ShortID},
	)
	childAfter, err := h.database.store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, childBefore.Revision, childAfter.Revision)
	assert.Equal(t, childBefore.Title, childAfter.Title)
	assert.Equal(t, childBefore.Body, childAfter.Body)
	assert.JSONEq(t, string(childBefore.Metadata), string(childAfter.Metadata))
	childCommentsAfter, err := h.database.store.CommentsByIssue(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, childCommentsBefore, childCommentsAfter)
	encodedCalls, err := json.Marshal(state.Calls)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedCalls), child.UID)
	assert.NotContains(t, string(encodedCalls), child.ShortID)
}

func testE2EPendingPublication(t *testing.T, executable string, backend e2eBackend) {
	t.Run("crash before reply does not mutate external state", func(t *testing.T) {
		h := newE2EHarness(t, executable, backend, true)
		h.createLocalComment(t, "Retry a request that never mutated")
		h.mutateState(func(current *fakeconnector.State) {
			current.Behavior.CrashBeforeReply["publish_comment"] = 1
		})
		_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
		require.Error(t, err)
		state := h.readState(t)
		assert.Zero(t, mutationCount(state, "publish_comment"))
		require.Len(t, state.Roots[0].Comments, 1, "crash-before must not create a native comment")
		h.restart(t)
		_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
		require.NoError(t, err)
		state = h.readState(t)
		assert.Equal(t, 1, callCount(state, "publish_comment"))
		assert.Equal(t, 1, mutationCount(state, "publish_comment"))
	})

	t.Run("crash after mutation replays the stable operation after restart", func(t *testing.T) {
		h := newE2EHarness(t, executable, backend, true)
		local := h.createLocalComment(t, "Publish once after restart")
		h.mutateState(func(current *fakeconnector.State) {
			current.Behavior.CrashAfterMutation["publish_comment"] = 1
		})
		_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
		require.Error(t, err)
		binding, readErr := h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
		require.NoError(t, readErr)
		assert.Equal(t, local.UID, binding.PendingCommentUID)
		assert.NotEmpty(t, binding.LastError)
		state := h.readState(t)
		assert.Equal(t, 1, mutationCount(state, "publish_comment"))
		require.Len(t, state.Roots[0].Comments, 2)

		h.restart(t)
		binding, err = h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
		require.NoError(t, err)
		assert.Equal(t, local.UID, binding.PendingCommentUID, "pending uncertainty must survive store restart")
		result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
		require.NoError(t, err)
		assert.Contains(t, eventTypes(result.Events), "issue.external_comment_resolved")
		binding, err = h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
		require.NoError(t, err)
		assert.Empty(t, binding.PendingCommentUID)
		mapping, err := h.database.store.ImportMappingBySource(
			t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), "comment", "published-0001",
		)
		require.NoError(t, err)
		require.NotNil(t, mapping.CommentID)
		assert.Equal(t, local.ID, *mapping.CommentID)
		state = h.readState(t)
		assert.Equal(t, 2, callCount(state, "publish_comment"))
		assert.Equal(t, 1, mutationCount(state, "publish_comment"))
		require.Len(t, state.Roots[0].Comments, 2)
	})

	for _, action := range []string{"adopt", "retry", "skip"} {
		t.Run("manual "+action, func(t *testing.T) {
			h := newE2EHarness(t, executable, backend, true)
			h.createLocalComment(t, "Resolve ambiguous publication")
			h.mutateState(func(current *fakeconnector.State) {
				current.Behavior.CrashAfterMutation["publish_comment"] = 1
			})
			_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
			require.Error(t, err)
			h.mutateState(func(current *fakeconnector.State) {
				first := current.Roots[0].Comments[len(current.Roots[0].Comments)-1]
				first.ID = "published-ambiguous"
				first.CreatedAt = first.CreatedAt.Add(time.Second)
				first.UpdatedAt = first.CreatedAt
				current.Roots[0].Comments = append(current.Roots[0].Comments, first)
				current.Behavior.Errors["publish_comment"] = connector.Error{
					Code: "temporary_unavailable", Message: "hold publication pending for manual resolution",
				}
			})
			h.restart(t)
			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
			var structured *connector.Error
			require.ErrorAs(t, err, &structured)
			assert.Equal(t, "temporary_unavailable", structured.Code)
			h.mutateState(func(current *fakeconnector.State) {
				delete(current.Behavior.Errors, "publish_comment")
			})

			params := ResolvePendingCommentParams{IssueID: h.issue.ID, Actor: "operator", Action: action}
			if action == "adopt" {
				params.ExternalCommentID = "published-0001"
			}
			resolved, _, err := h.service.ResolvePendingComment(t.Context(), params)
			require.NoError(t, err)
			if action == "retry" {
				assert.NotEmpty(t, resolved.PendingCommentUID)
			} else {
				assert.Empty(t, resolved.PendingCommentUID)
			}
			before := mutationCount(h.readState(t), "publish_comment")
			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
			require.NoError(t, err)
			binding, readErr := h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Empty(t, binding.PendingCommentUID)
			after := mutationCount(h.readState(t), "publish_comment")
			assert.Equal(t, before, after)
		})
	}
}

func testE2EPlanningFields(t *testing.T, executable string, backend e2eBackend) {
	h := newE2EHarness(t, executable, backend, false)
	_, err := h.service.MapField(t.Context(), MapFieldParams{
		ConnectorInstance: "example", KataField: "scheduled_on", ExternalField: "field-schedule",
	})
	require.NoError(t, err)
	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Zero(t, first.FieldConflicts)
	issue, err := h.database.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"scheduled_on":"2026-08-21T09:00:00","timezone":"Europe/Paris"}`, string(issue.Metadata))

	_, err = h.database.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: h.issue.ID, Actor: "operator", Patch: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-08-22T09:00:00"`),
			"timezone":     json.RawMessage(`"Europe/Paris"`),
		},
	})
	require.NoError(t, err)
	h.mutateState(func(current *fakeconnector.State) {
		current.Roots[0].Fields["field-schedule"] = connector.FieldValue{
			Kind: "local_datetime", Value: "2026-08-23T09:00:00", Timezone: "Europe/Paris",
		}
	})
	conflicted, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, conflicted.FieldConflicts)
	states, err := h.database.store.ExternalFieldStates(t.Context(), h.binding.ID)
	require.NoError(t, err)
	require.Len(t, states, 1)
	assert.True(t, states[0].Conflicted)

	_, _, _, err = h.database.store.CloseIssue(
		t.Context(), h.issue.ID, "done", "verifier", "Verified with field conflict.",
		[]db.Evidence{{Type: "test", Command: "go test ./internal/rootbridge"}},
	)
	require.NoError(t, err)
	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FieldConflicts)
	assert.Equal(t, "complete", h.readState(t).Roots[0].Root.State, "field conflict must not block completion")

	resolved, _, err := h.service.ResolveFieldConflict(t.Context(), h.issue.ID, "scheduled_on", "external", "operator")
	require.NoError(t, err)
	assert.False(t, resolved.Conflicted)
	issue, err = h.database.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"scheduled_on":"2026-08-23T09:00:00","timezone":"Europe/Paris"}`, string(issue.Metadata))
}

func testE2EPlanningFieldShapes(t *testing.T, executable string, backend e2eBackend) {
	for _, test := range []struct {
		kataField string
		kind      string
		external  connector.FieldValue
		metadata  string
	}{
		{kataField: "scheduled_on", kind: "date", external: connector.FieldValue{Kind: "date", Value: "2026-08-24"}, metadata: `{"scheduled_on":"2026-08-24"}`},
		{kataField: "scheduled_on", kind: "local", external: connector.FieldValue{Kind: "local_datetime", Value: "2026-08-24T09:30:00", Timezone: "Europe/Paris"}, metadata: `{"scheduled_on":"2026-08-24T09:30:00","timezone":"Europe/Paris"}`},
		{kataField: "scheduled_on", kind: "instant", external: connector.FieldValue{Kind: "instant", Value: "2026-08-24T07:30:00.12Z"}, metadata: `{"scheduled_on":"2026-08-24T07:30:00.12Z"}`},
		{kataField: "deadline_on", kind: "date", external: connector.FieldValue{Kind: "date", Value: "2026-08-25"}, metadata: `{"deadline_on":"2026-08-25"}`},
		{kataField: "deadline_on", kind: "local", external: connector.FieldValue{Kind: "local_datetime", Value: "2026-08-25T10:45:00", Timezone: "Europe/Paris"}, metadata: `{"deadline_on":"2026-08-25T10:45:00","timezone":"Europe/Paris"}`},
		{kataField: "deadline_on", kind: "instant", external: connector.FieldValue{Kind: "instant", Value: "2026-08-25T08:45:00.12Z"}, metadata: `{"deadline_on":"2026-08-25T08:45:00.12Z"}`},
	} {
		t.Run(test.kataField+" "+test.kind, func(t *testing.T) {
			h := newE2EHarness(t, executable, backend, false)
			h.mutateState(func(current *fakeconnector.State) {
				current.Roots[0].Fields["field-schedule"] = test.external
			})
			_, err := h.service.MapField(t.Context(), MapFieldParams{
				ConnectorInstance: "example", KataField: test.kataField, ExternalField: "field-schedule",
			})
			require.NoError(t, err)
			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
			require.NoError(t, err)
			issue, err := h.database.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			assert.JSONEq(t, test.metadata, string(issue.Metadata))

			patch := map[string]json.RawMessage{test.kataField: json.RawMessage("null")}
			if test.kind == "local" {
				patch["timezone"] = json.RawMessage("null")
			}
			_, err = h.database.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
				IssueID: h.issue.ID, Actor: "operator", Patch: patch,
			})
			require.NoError(t, err)
			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
			require.NoError(t, err)
			assert.Equal(t, connector.FieldValue{Kind: "null"}, h.readState(t).Roots[0].Fields["field-schedule"])
		})
	}
}

func testE2ERestorePause(t *testing.T, executable string, backend e2eBackend) {
	source := newE2EHarness(t, executable, backend, true)
	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source.database.store, &exported, jsonl.ExportOptions{IncludeDeleted: true}))

	target := backend.open(t)
	t.Cleanup(func() { target.close(t) })
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(exported.Bytes()), target.store))
	binding, err := target.store.ExternalRootBindingByIssue(t.Context(), source.issue.ID)
	require.NoError(t, err)
	assert.False(t, binding.Enabled)
	assert.Equal(t, "restore_reconfirmation_required", binding.PausedReason)

	stateBefore := source.readState(t)
	_, reconciler, service := wireE2E(t, target.store, executable, source.statePath, source.now)
	_, err = reconciler.Run(t.Context(), binding.ID, RunOptions{Manual: true})
	require.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	stateAfter := source.readState(t)
	assert.Equal(t, len(stateBefore.Calls), len(stateAfter.Calls), "paused restore must not call the connector")

	source.mutateState(func(current *fakeconnector.State) {
		current.Description.AccountIdentity = "account-drifted"
	})
	_, _, err = service.Resume(t.Context(), binding.IssueID, "operator")
	require.ErrorIs(t, err, ErrConnectorIdentityChanged)
	stillPaused, err := target.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, err)
	assert.False(t, stillPaused.Enabled)

	source.mutateState(func(current *fakeconnector.State) {
		current.Description.AccountIdentity = "account-example"
		current.Description.Capabilities = []connector.Capability{
			connector.CapabilityFields, connector.CapabilityConditionalFields,
		}
	})
	_, _, err = service.Resume(t.Context(), binding.IssueID, "operator")
	require.ErrorIs(t, err, ErrCommentPublishingUnavailable)
	stillPaused, err = target.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, err)
	assert.False(t, stillPaused.Enabled)

	source.mutateState(func(current *fakeconnector.State) {
		current.Description.Capabilities = []connector.Capability{
			connector.CapabilityFields, connector.CapabilityConditionalFields, connector.CapabilityPublishComment,
		}
	})
	resumed, _, err := service.Resume(t.Context(), binding.IssueID, "operator")
	require.NoError(t, err)
	assert.True(t, resumed.Enabled)
	assert.Empty(t, resumed.PausedReason)
	_, err = reconciler.Run(t.Context(), binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
}

func testE2EFailureIsolation(t *testing.T, executable string, backend e2eBackend) {
	h := newE2EHarness(t, executable, backend, false)
	h.mutateState(func(current *fakeconnector.State) {
		current.Behavior.CrashBeforeReply["read_root"] = 1
	})
	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.ErrorIs(t, err, ErrConnectorCall)
	require.ErrorIs(t, err, connectorclient.ErrProcessFailure)
	assert.Equal(t, "external connector process failed", err.Error())
	binding, readErr := h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "external connector process failed", binding.LastError)
	assert.NotContains(t, binding.LastError, h.statePath)
	h.restart(t)
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err, "one connector process crash must not stop later reconciliation")

	h.mutateState(func(current *fakeconnector.State) {
		current.Behavior.ResponseProtocol = "unsupported.connector.protocol"
	})
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.ErrorIs(t, err, ErrConnectorCall)
	require.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
	assert.Equal(t, "external connector protocol failed", err.Error())
	binding, readErr = h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "external connector protocol failed", binding.LastError)
	assert.NotContains(t, binding.LastError, "unsupported.connector.protocol")
	h.mutateState(func(current *fakeconnector.State) {
		current.Behavior.ResponseProtocol = ""
		current.Behavior.Errors["read_root"] = connector.Error{
			Code: "temporary_unavailable", Message: "synthetic external failure",
		}
	})
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	var structured *connector.Error
	require.ErrorAs(t, err, &structured)
	assert.Equal(t, "temporary_unavailable", structured.Code)
	assert.Equal(t, "synthetic external failure", structured.Message)
	h.mutateState(func(current *fakeconnector.State) {
		delete(current.Behavior.Errors, "read_root")
	})
	h.restart(t)
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.NoError(t, err)
	binding, err = h.database.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.Empty(t, binding.ClaimToken)
	assert.NotNil(t, binding.LastSuccessAt)
}

type e2eBackend struct {
	name string
	open func(*testing.T) *e2eDatabase
}

type e2eDatabase struct {
	store   db.Storage
	reopen  func(context.Context) (db.Storage, error)
	cleanup func(context.Context) error
}

func (d *e2eDatabase) restart(t *testing.T) {
	t.Helper()
	require.NoError(t, closeStorage(d.store))
	store, err := d.reopen(t.Context())
	require.NoError(t, err)
	d.store = store
}

func (d *e2eDatabase) close(t *testing.T) {
	t.Helper()
	if d.store != nil {
		require.NoError(t, closeStorage(d.store))
		d.store = nil
	}
	if d.cleanup != nil {
		require.NoError(t, d.cleanup(context.Background()))
		d.cleanup = nil
	}
}

func closeStorage(store db.Storage) error {
	if closer, ok := store.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func openSQLiteE2E(t *testing.T) *e2eDatabase {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kata.db")
	open := func(ctx context.Context) (db.Storage, error) { return sqlitestore.Open(ctx, path) }
	store, err := open(t.Context())
	require.NoError(t, err)
	return &e2eDatabase{store: store, reopen: open}
}

func TestPostgresE2ESchemaNamesAndQuoting(t *testing.T) {
	const total = 512
	seen := make(map[string]struct{}, total)
	for range total {
		name, err := newPostgresE2ESchema()
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(name, "rootbridge_e2e_"))
		require.Len(t, name, len("rootbridge_e2e_")+32)
		_, err = hex.DecodeString(strings.TrimPrefix(name, "rootbridge_e2e_"))
		require.NoError(t, err)
		_, duplicate := seen[name]
		require.False(t, duplicate, "duplicate PostgreSQL schema %q", name)
		seen[name] = struct{}{}
	}
	assert.Equal(t, `"rootbridge_e2e_example"`, quotePostgresIdentifier("rootbridge_e2e_example"))
	assert.Equal(t, `"rootbridge_e2e_""example"`, quotePostgresIdentifier(`rootbridge_e2e_"example`))
}

func newPostgresE2ESchema() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate PostgreSQL test schema suffix: %w", err)
	}
	return "rootbridge_e2e_" + hex.EncodeToString(random[:]), nil
}

func quotePostgresIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func openPostgresE2E(t *testing.T, dsn string) *e2eDatabase {
	t.Helper()
	schema, err := newPostgresE2ESchema()
	require.NoError(t, err)
	open := func(ctx context.Context) (db.Storage, error) {
		return pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	}
	store, err := open(t.Context())
	require.NoError(t, err)
	cleanup := func(ctx context.Context) error {
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			return fmt.Errorf("open PostgreSQL cleanup connection: %w", err)
		}
		defer func() { _ = admin.Close() }()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+quotePostgresIdentifier(schema)+` CASCADE`); err != nil {
			return fmt.Errorf("drop test-owned PostgreSQL schema: %w", err)
		}
		return nil
	}
	return &e2eDatabase{store: store, reopen: open, cleanup: cleanup}
}

type e2eHarness struct {
	t          *testing.T
	executable string
	backend    e2eBackend
	database   *e2eDatabase
	statePath  string
	base       time.Time
	now        time.Time
	registry   *Registry
	reconciler *Reconciler
	service    *Service
	project    db.Project
	issue      db.Issue
	binding    db.ExternalRootBinding
}

func newE2EHarness(t *testing.T, executable string, backend e2eBackend, publish bool) *e2eHarness {
	t.Helper()
	base := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "connector-state.json")
	writeE2EState(t, statePath, initialE2EState(base))
	database := backend.open(t)
	h := &e2eHarness{
		t: t, executable: executable, backend: backend, database: database,
		statePath: statePath, base: base, now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	}
	t.Cleanup(func() { database.close(t) })
	h.wire(t)
	project, err := database.store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := database.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Local title", Body: "Local body", Author: "operator",
	})
	require.NoError(t, err)
	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "example",
		Locator: "fixture-root", Actor: "operator", PublishComments: publish,
	})
	require.NoError(t, err)
	h.project, h.issue, h.binding = project, issue, binding
	return h
}

func (h *e2eHarness) wire(t *testing.T) {
	t.Helper()
	h.registry, h.reconciler, h.service = wireE2E(t, h.database.store, h.executable, h.statePath, h.now)
}

func wireE2E(
	t *testing.T,
	store db.Storage,
	executable string,
	statePath string,
	now time.Time,
) (*Registry, *Reconciler, *Service) {
	t.Helper()
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "example", Command: executable, Args: []string{"--state", statePath}, TimeoutSeconds: 10,
	}}, nil)
	require.NoError(t, err)
	reconciler := NewReconciler(store, registry, ReconcilerConfig{Now: func() time.Time { return now }})
	service := NewService(store, registry, func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, bindingID, RunOptions{ClaimToken: claimToken})
		return result.Events, err
	})
	return registry, reconciler, service
}

func (h *e2eHarness) restart(t *testing.T) {
	t.Helper()
	h.database.restart(t)
	h.wire(t)
}

func (h *e2eHarness) createLocalComment(t *testing.T, body string) db.Comment {
	t.Helper()
	comment, _, err := h.database.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "operator", Body: body,
	})
	require.NoError(t, err)
	return comment
}

func (h *e2eHarness) readState(t *testing.T) fakeconnector.State {
	t.Helper()
	current, err := fakeconnector.Load(h.statePath)
	require.NoError(t, err)
	require.NoError(t, fakeconnector.AuditExternalSurface(
		current, "root-example", []string{h.issue.UID}, []string{h.issue.ShortID},
	))
	return current
}

func (h *e2eHarness) mutateState(apply func(*fakeconnector.State)) {
	h.t.Helper()
	current := h.readState(h.t)
	if current.Behavior.CrashBeforeReply == nil {
		current.Behavior.CrashBeforeReply = make(map[string]int)
	}
	if current.Behavior.CrashAfterMutation == nil {
		current.Behavior.CrashAfterMutation = make(map[string]int)
	}
	if current.Behavior.Errors == nil {
		current.Behavior.Errors = make(map[string]connector.Error)
	}
	apply(&current)
	writeE2EState(h.t, h.statePath, current)
}

func initialE2EState(base time.Time) fakeconnector.State {
	return fakeconnector.State{
		Description: connector.Description{
			ConnectorID: "example.connector", DisplayName: "Example Connector", Protocol: connector.ProtocolVersion,
			Capabilities: []connector.Capability{
				connector.CapabilityFields, connector.CapabilityConditionalFields, connector.CapabilityPublishComment,
			},
			ConfigSchema: []byte(`{"type":"object","additionalProperties":false}`), SelfActorID: "actor-self",
			AccountIdentity: "account-example",
		},
		Roots: []fakeconnector.StoredRoot{{
			Locator: "fixture-root",
			Root: connector.Root{
				Key: "root-example", IdentityKey: "account-example", Title: "External title", Body: "External body",
				State: "open", Revision: "revision-initial", UpdatedAt: base, ObservedAt: base,
			},
			Comments: []connector.Comment{{
				ID: "comment-before-bind", Revision: "revision-comment-before-bind", Body: "Historical external note",
				Author:    connector.Actor{ID: "actor-history", DisplayName: "Historical Reviewer"},
				CreatedAt: base.Add(-time.Minute), UpdatedAt: base.Add(-time.Minute),
			}},
			Fields: map[string]connector.FieldValue{
				"field-schedule": {Kind: "local_datetime", Value: "2026-08-21T09:00:00", Timezone: "Europe/Paris"},
			},
		}},
		Fields: []connector.FieldDescriptor{{
			ID: "field-schedule", DisplayName: "Schedule", AcceptedKinds: []string{"date", "instant", "local_datetime"},
			Nullable: true, Writable: true, SchemaRevision: "schema-1",
		}},
		Behavior: fakeconnector.Behavior{
			CrashBeforeReply: make(map[string]int), CrashAfterMutation: make(map[string]int),
			Errors: make(map[string]connector.Error),
		},
	}
}

func writeE2EState(t *testing.T, path string, current fakeconnector.State) {
	t.Helper()
	require.NoError(t, fakeconnector.Write(path, current))
}

func buildFakeConnector(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "fake-connector")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	command := exec.Command("go", "build", "-o", executable, "../connector/fakeconnector/cmd") // #nosec G204 -- fixed test tool and package; only the test-owned output path varies.
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return executable
}

func mutationCount(current fakeconnector.State, method string) int {
	count := 0
	for _, mutation := range current.Mutations {
		if mutation.Method == method {
			count++
		}
	}
	return count
}

func callCount(current fakeconnector.State, method string) int {
	count := 0
	for _, call := range current.Calls {
		if call.Method == method {
			count++
		}
	}
	return count
}

func assertExternalCallsContainNoKataOrChildIdentity(
	t *testing.T,
	current fakeconnector.State,
	longUIDs []string,
	shortIDs []string,
) {
	t.Helper()
	require.NotEmpty(t, current.Calls, "fixture must record the executable request surface")
	require.NoError(t, fakeconnector.AuditExternalSurface(current, "root-example", longUIDs, shortIDs))
}
