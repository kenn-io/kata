package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// scopedEnvelope is the wire shape of one event envelope shared by poll
// responses and SSE frames.
type scopedEnvelope struct {
	EventID           int64           `json:"event_id"`
	OriginInstanceUID string          `json:"origin_instance_uid"`
	Type              string          `json:"type"`
	IssueUID          *string         `json:"issue_uid"`
	ContentHash       string          `json:"content_hash"`
	Payload           json.RawMessage `json:"payload"`
}

func TestIssueScopedEventsResetForHiddenInitialLinks(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		t.Run(fmt.Sprintf("incoming=%t", incoming), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			other, err := env.DB.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
			newScopedTokens(t, env, project, root)
			afterID, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			stream := openSSE(t, env, "after_id="+strconv.FormatInt(afterID, 10),
				http.Header{"Authorization": {"Bearer worker-token"}})
			defer func() { _ = stream.Body.Close() }()
			framer := newSSEFramer(stream.Body)

			resp, body := envDoRaw(t, env, http.MethodPost, scopedProjectPath(other.ID, "issues"),
				map[string]any{
					"actor": "coordinator", "title": "Hidden linked work",
					"links": []map[string]any{{"type": "blocks", "to_ref": root.UID, "incoming": incoming}},
				}, map[string]string{"Authorization": "Bearer coordinator-token"})
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			var created struct {
				Issue db.Issue `json:"issue"`
				Event db.Event `json:"event"`
			}
			require.NoError(t, json.Unmarshal(body, &created))
			for _, path := range []string{"/api/v1/events", scopedProjectPath(project.ID, "events")} {
				resp, body = envDoRaw(t, env, http.MethodGet, path+"?after_id="+strconv.FormatInt(afterID, 10), nil,
					map[string]string{"Authorization": "Bearer worker-token"})
				require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
				var polled api.PollEventsResponse
				require.NoError(t, json.Unmarshal(body, &polled.Body))
				require.True(t, polled.Body.ResetRequired, "initial links must invalidate the visible endpoint")
				require.Equal(t, created.Event.ID, polled.Body.ResetAfterID)
				require.Empty(t, polled.Body.Events)
				require.NotContains(t, string(body), created.Issue.UID)
				require.NotContains(t, string(body), other.UID)
			}
			frame, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok, "live stream must reset for the initial link")
			require.Equal(t, "sync.reset_required", frame.event)
			require.NotContains(t, frame.data, created.Issue.UID)
		})
	}
}

func TestIssueScopedPollResetsForItsProjectRename(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct {
		project db.Project
		reset   bool
	}{{other, false}, {project, true}} {
		_, event, _, err := env.DB.RenameProjectAndEvent(t.Context(), tc.project.ID, tc.project.Name+"-renamed", "coordinator")
		require.NoError(t, err)
		resp, body := envDoRaw(t, env, http.MethodGet, scopedProjectPath(project.ID, "events")+"?after_id="+strconv.FormatInt(afterID, 10), nil,
			map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var polled api.PollEventsResponse
		require.NoError(t, json.Unmarshal(body, &polled.Body))
		require.Equal(t, tc.reset, polled.Body.ResetRequired)
		require.Equal(t, event.ID, polled.Body.NextAfterID)
		require.Empty(t, polled.Body.Events)
		require.NotContains(t, string(body), tc.project.Name)
		afterID = event.ID
	}
}

func TestIssueScopedLifecycleEventsSurvivePollHistoryAndDigest(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, other.ID, "Incoming child", &root)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: child.ID, FromProjectID: other.ID, ToProjectID: project.ID,
		IfMatchRev: child.Revision, Actor: "coordinator",
	})
	require.NoError(t, err)
	_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), child.ID, "coordinator")
	require.NoError(t, err)
	_, _, _, err = env.DB.RestoreIssue(t.Context(), child.ID, "coordinator")
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	for _, path := range []string{
		"/api/v1/events?after_id=" + strconv.FormatInt(afterID, 10),
		"/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + child.UID + "&include_history=true",
	} {
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil, headers)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var response struct {
			ResetRequired bool             `json:"reset_required"`
			Events        []scopedEnvelope `json:"events"`
			Selected      struct {
				History []db.Event `json:"history"`
			} `json:"selected"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.False(t, response.ResetRequired)
		events := response.Events
		for _, event := range response.Selected.History {
			events = append(events, scopedEnvelope{
				Type: event.Type, Payload: json.RawMessage(event.Payload),
				OriginInstanceUID: event.OriginInstanceUID, ContentHash: event.ContentHash,
			})
		}
		payloads := make(map[string]map[string]any)
		for _, event := range events {
			requireScopedEnvelopeRedacted(t, event)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			payloads[event.Type] = payload
		}
		for eventType, keys := range map[string][]string{
			"issue.moved":        {"updated_at"},
			"issue.soft_deleted": {"deleted_at"},
			"issue.restored":     {"restored_at", "updated_at"},
		} {
			require.Contains(t, payloads, eventType)
			require.Len(t, payloads[eventType], len(keys))
			for _, key := range keys {
				require.NotEmpty(t, payloads[eventType][key])
			}
		}
		require.NotContains(t, string(body), other.UID)
	}
	resp, body := envDoRaw(t, env, http.MethodGet, scopedProjectPath(project.ID, "digest")+"?since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var digest api.DigestResponse
	require.NoError(t, json.Unmarshal(body, &digest.Body))
	require.Equal(t, 1, digest.Body.Totals.Deleted)
	require.Equal(t, 1, digest.Body.Totals.Restored)
}

func newScopedTokens(t *testing.T, env *testenv.Env, project db.Project, root db.Issue) {
	t.Helper()
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
}

func scopedProjectPath(projectID int64, suffix string) string {
	return "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/" + suffix
}

func requireScopedEnvelopeRedacted(t *testing.T, event scopedEnvelope) {
	t.Helper()
	require.Empty(t, event.ContentHash, "scoped events must not carry content_hash")
	require.Empty(t, event.OriginInstanceUID,
		"scoped events must not carry origin_instance_uid (infrastructure identity)")
}

// Claim 1 + 3: catch-up and live scoped event output must both carry the
// typed safe projection — no content_hash, no origin_instance_uid, and a
// payload restricted to the event type's allowlist.
func TestIssueScopedPollAndLiveSSEUseTypedProjection(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	workerHeaders := map[string]string{"Authorization": "Bearer worker-token"}
	stream := openSSE(t, env,
		"after_id="+strconv.FormatInt(afterID, 10),
		http.Header{"Authorization": []string{"Bearer worker-token"}})
	defer func() { _ = stream.Body.Close() }()
	framer := newSSEFramer(stream.Body)

	resp, body := envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/comments"),
		map[string]any{"actor": "coordinator", "body": "status note"},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	// Live phase: the comment must arrive through the same typed projection
	// as catch-up.
	frame, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "live scoped stream should deliver the comment event")
	require.Equal(t, "issue.commented", frame.event)
	var liveEnvelope scopedEnvelope
	require.NoError(t, json.Unmarshal([]byte(frame.data), &liveEnvelope))
	requireScopedEnvelopeRedacted(t, liveEnvelope)
	var livePayload map[string]any
	require.NoError(t, json.Unmarshal(liveEnvelope.Payload, &livePayload))
	for key := range livePayload {
		require.Contains(t,
			[]string{"comment_uid", "author", "body", "created_at"}, key,
			"live scoped payload must be the typed projection")
	}

	// Catch-up: every polled event envelope must be redacted too.
	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id=0&limit=100", nil, workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		Events []scopedEnvelope `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.NotEmpty(t, polled.Events)
	for _, event := range polled.Events {
		requireScopedEnvelopeRedacted(t, event)
	}
	require.NotContains(t, string(body), "origin_instance_uid\":\"0")
}

// Claim 1 (tail): an in-scope event whose payload cannot be safely projected
// (unknown issue.* type) must fail closed with sync.reset_required instead of
// surfacing as a hollow event frame.
func TestIssueScopedPollResetsOnUnprojectableEvent(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	// Simulate an event written by a future/unknown producer: an issue.*
	// type the scoped projection has no typed mapping for.
	_, err = env.DB.ExecContext(t.Context(), `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
			issue_id, issue_uid, type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		VALUES('01ARZ3NDEKTSV4RRFFQ69G5FAV', '01ARZ3NDEKTSV4RRFFQ69G5FAW', ?, ?,
			?, ?, 'issue.future_mutation', 'coordinator', '{"title":"secret"}', 1, 0, ?)`,
		project.ID, project.Name, child.ID, child.UID, strings.Repeat("a", 64))
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		ResetAfterID  int64            `json:"reset_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.True(t, polled.ResetRequired,
		"an unprojectable in-scope event must force a scoped reset")
	require.Empty(t, polled.Events)
	require.NotContains(t, string(body), "secret")
}

// Claim 2: every event-bearing issue-scoped mutation response must carry the
// typed safe projection, including multi-event PATCH batches and idempotent
// replay receipts. Unscoped responses keep their raw events.
func TestIssueScopedMutationResponsesProjectEvents(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	newScopedTokens(t, env, project, root)
	workerHeaders := map[string]string{"Authorization": "Bearer worker-token"}

	// Assignment is a separate ownership handler and must obey the same
	// response boundary as the generic action handlers.
	resp, body := envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/actions/assign"),
		map[string]any{"actor": "worker-a", "owner": "worker-a"}, workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var assignResp struct {
		Event *scopedEnvelope `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &assignResp))
	require.NotNil(t, assignResp.Event)
	requireScopedEnvelopeRedacted(t, *assignResp.Event)

	// Multi-event PATCH batch (issue.updated): the events array and the
	// compatibility event alias must both be projected.
	transport := struct {
		Events []scopedEnvelope `json:"events"`
		Event  *scopedEnvelope  `json:"event"`
	}{}
	resp, body = envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID,
		"issues/"+child.ShortID),
		map[string]any{"actor": "worker-a", "title": "Renamed child"},
		workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.NoError(t, json.Unmarshal(body, &transport))
	require.NotEmpty(t, transport.Events)
	for _, event := range transport.Events {
		requireScopedEnvelopeRedacted(t, event)
	}
	requireScopedEnvelopeRedacted(t, *transport.Event)

	// Keyed comment: the stored issue.commented payload carries the caller's
	// idempotency key and fingerprint; the scoped response must strip them.
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/comments"),
		map[string]any{"actor": "worker-a", "body": "progress note", "teammate": "reviewer-7"},
		map[string]string{
			"Authorization":   "Bearer worker-token",
			"Idempotency-Key": "comment-retry-key",
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var commentResp struct {
		Event *scopedEnvelope `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &commentResp))
	require.NotNil(t, commentResp.Event)
	requireScopedEnvelopeRedacted(t, *commentResp.Event)
	require.NotContains(t, string(commentResp.Event.Payload), "idempotency_key")
	require.NotContains(t, string(commentResp.Event.Payload), "idempotency_fingerprint")
	var commentPayload struct {
		Teammate string `json:"teammate"`
	}
	var commentPayloadJSON string
	require.NoError(t, json.Unmarshal(commentResp.Event.Payload, &commentPayloadJSON))
	require.NoError(t, json.Unmarshal([]byte(commentPayloadJSON), &commentPayload))
	require.Equal(t, "reviewer-7", commentPayload.Teammate)

	// Idempotent create replay: original_event is the stored issue.created
	// event and must go through the same projection.
	createBody := func() map[string]any {
		return map[string]any{
			"actor": "worker-a", "title": "Scoped replay child",
			"links": []map[string]any{{"type": "parent", "to_ref": root.ShortID}},
		}
	}
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues"),
		createBody(), map[string]string{
			"Authorization":   "Bearer worker-token",
			"Idempotency-Key": "create-retry-key",
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var fresh struct {
		Event *scopedEnvelope `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &fresh))
	require.NotNil(t, fresh.Event)
	requireScopedEnvelopeRedacted(t, *fresh.Event)
	require.NotContains(t, string(fresh.Event.Payload), "idempotency_key")
	require.NotContains(t, string(fresh.Event.Payload), "idempotency_fingerprint")

	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues"),
		createBody(), map[string]string{
			"Authorization":   "Bearer worker-token",
			"Idempotency-Key": "create-retry-key",
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var replay struct {
		OriginalEvent *scopedEnvelope `json:"original_event"`
	}
	require.NoError(t, json.Unmarshal(body, &replay))
	require.NotNil(t, replay.OriginalEvent, "replay must carry original_event")
	requireScopedEnvelopeRedacted(t, *replay.OriginalEvent)
	require.NotContains(t, string(replay.OriginalEvent.Payload), "idempotency_key")
	require.NotContains(t, string(replay.OriginalEvent.Payload), "idempotency_fingerprint")

	// Unscoped responses are unchanged: the coordinator still receives the
	// raw stored event with infrastructure identity intact.
	resp, body = envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID,
		"issues/"+child.ShortID),
		map[string]any{"actor": "coordinator", "title": "Coordinator rename"},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var unscoped struct {
		Events []scopedEnvelope `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &unscoped))
	require.NotEmpty(t, unscoped.Events)
	require.NotEmpty(t, unscoped.Events[0].ContentHash,
		"unscoped mutation responses keep raw events")
	require.NotEmpty(t, unscoped.Events[0].OriginInstanceUID,
		"unscoped mutation responses keep raw events")
}

// Regression for the typed scoped projection omitting issue.soft_deleted,
// issue.restored, and issue.moved: authorized lifecycle and move events
// vanished from scoped outputs — polling degraded to sync.reset_required,
// UI history and digest/audit report projections dropped the rows. The
// scoped client must instead receive the typed safe payloads, and a move
// into the granted project must not leak the source project's identity.
func TestIssueScopedLifecycleAndMoveEventsReachScopedClients(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	hub, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	arrival := createScopedHTTPTestIssue(t, env, hub.ID, "Arrival", nil)
	newScopedTokens(t, env, project, root)
	workerHeaders := map[string]string{"Authorization": "Bearer worker-token"}
	coordinatorHeaders := map[string]string{"Authorization": "Bearer coordinator-token"}

	// Close the child so the scoped audit projection has an authorized close
	// row that must survive alongside the lifecycle events.
	resp, body := envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/actions/close"),
		map[string]any{
			"actor": "coordinator", "reason": "done",
			"message":  "Scoped close retained alongside lifecycle events.",
			"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
		}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	// Soft delete then restore the child as an unscoped coordinator.
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/actions/delete"),
		map[string]any{"actor": "coordinator"},
		map[string]string{
			"Authorization":  "Bearer coordinator-token",
			"X-Kata-Confirm": "DELETE example-project#" + child.ShortID,
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+child.ShortID+"/actions/restore"),
		map[string]any{"actor": "coordinator"}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	// Move the arrival issue into the scoped project, then attach it to the
	// granted subtree. Once linked, the move is authorized history for the
	// scoped client rather than a hidden membership change.
	resp, body = envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(hub.ID, 10)+
			"/issues/"+arrival.ShortID+"/actions/move",
		map[string]any{"actor": "coordinator", "to_project_uid": project.UID},
		map[string]string{
			"Authorization": "Bearer coordinator-token",
			"If-Match":      fmt.Sprintf(`"rev-%d"`, arrival.Revision),
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var moveResp struct {
		NewShortID string `json:"new_short_id"`
	}
	require.NoError(t, json.Unmarshal(body, &moveResp))
	moved, err := env.DB.IssueByID(t.Context(), arrival.ID)
	require.NoError(t, err)
	_, _, err = env.DB.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{
		FromIssueID: moved.ID, ToIssueID: root.ID, Type: "parent", Author: "coordinator",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: moved.ID,
		FromShortID: moved.ShortID, FromUID: moved.UID,
		ToShortID: root.ShortID, ToUID: root.UID, Actor: "coordinator",
	})
	require.NoError(t, err)

	// Polling: the lifecycle and move events must arrive with typed safe
	// payloads instead of forcing a scoped reset.
	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id=0&limit=200", nil, workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		Events        []scopedEnvelope `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.False(t, polled.ResetRequired,
		"authorized lifecycle/move events must project, not force a scoped reset")
	byType := map[string][]scopedEnvelope{}
	for _, event := range polled.Events {
		requireScopedEnvelopeRedacted(t, event)
		byType[event.Type] = append(byType[event.Type], event)
	}
	require.Len(t, byType["issue.soft_deleted"], 1, "soft delete must reach the scoped poll")
	require.Len(t, byType["issue.restored"], 1, "restore must reach the scoped poll")
	require.Len(t, byType["issue.moved"], 1, "move into the granted project must reach the scoped poll")

	var deletedPayload map[string]any
	require.NoError(t, json.Unmarshal(byType["issue.soft_deleted"][0].Payload, &deletedPayload))
	require.Equal(t, map[string]any{"deleted_at": deletedPayload["deleted_at"]}, deletedPayload)
	require.NotEmpty(t, deletedPayload["deleted_at"])

	var restoredPayload map[string]any
	require.NoError(t, json.Unmarshal(byType["issue.restored"][0].Payload, &restoredPayload))
	require.Len(t, restoredPayload, 2)
	require.NotEmpty(t, restoredPayload["restored_at"])
	require.NotEmpty(t, restoredPayload["updated_at"])

	var movedPayload map[string]any
	require.NoError(t, json.Unmarshal(byType["issue.moved"][0].Payload, &movedPayload))
	require.Equal(t, map[string]any{
		"to_project_uid": project.UID,
		"to_short_id":    moveResp.NewShortID,
		"updated_at":     movedPayload["updated_at"],
	}, movedPayload)
	require.NotEmpty(t, movedPayload["updated_at"])
	require.NotContains(t, string(byType["issue.moved"][0].Payload), "from_",
		"scoped move payloads must not carry source-project fields")
	require.NotContains(t, string(body), hub.UID,
		"scoped move payloads must not leak the source project UID")

	// History: the scoped UI snapshot must retain the lifecycle events for
	// the restored child and the safe move payload for the arrival issue.
	for _, selected := range []struct {
		uid      string
		wantType []string
	}{
		{uid: child.UID, wantType: []string{"issue.closed", "issue.soft_deleted", "issue.restored"}},
		{uid: moved.UID, wantType: []string{"issue.moved"}},
	} {
		resp, body = envDoRaw(t, env, http.MethodGet,
			"/api/v1/ui/snapshot?view=all-open&selected_issue_uid="+selected.uid+"&include_history=true",
			nil, workerHeaders)
		require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		var snapshot struct {
			Selected struct {
				History []db.Event `json:"history"`
			} `json:"selected"`
		}
		require.NoError(t, json.Unmarshal(body, &snapshot))
		historyTypes := map[string]int{}
		for _, event := range snapshot.Selected.History {
			historyTypes[event.Type]++
			require.Empty(t, event.ContentHash, "scoped history must drop content_hash")
			require.Empty(t, event.OriginInstanceUID, "scoped history must drop origin_instance_uid")
		}
		for _, want := range selected.wantType {
			require.Equal(t, 1, historyTypes[want],
				"scoped history for %s must retain %s", selected.uid, want)
		}
		require.NotContains(t, string(body), hub.UID, "scoped history must not leak the source project UID")
	}

	// Digest: the scoped report projection must count the lifecycle events
	// and the move instead of dropping them.
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	resp, body = envDoRaw(t, env, http.MethodGet,
		scopedProjectPath(project.ID, "digest")+"?since="+since, nil, workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var digest struct {
		Totals struct {
			Deleted  int `json:"deleted"`
			Restored int `json:"restored"`
			Other    int `json:"other"`
		} `json:"totals"`
	}
	require.NoError(t, json.Unmarshal(body, &digest))
	require.Equal(t, 1, digest.Totals.Deleted, "scoped digest must count the soft delete")
	require.Equal(t, 1, digest.Totals.Restored, "scoped digest must count the restore")
	require.Equal(t, 1, digest.Totals.Other, "scoped digest must count the move")
	require.NotContains(t, string(body), hub.UID, "scoped digest must not leak the source project UID")

	// Audit: the shared scoped report filter must keep the authorized close
	// row while the lifecycle and move events flow through it.
	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/audit/closes?project_id="+strconv.FormatInt(project.ID, 10), nil, workerHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var audit struct {
		Rows []struct {
			Issue  string `json:"issue"`
			Reason string `json:"reason"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(body, &audit))
	require.Len(t, audit.Rows, 1)
	require.Equal(t, child.ShortID, audit.Rows[0].Issue)
	require.Equal(t, "done", audit.Rows[0].Reason)
}

// Claim 4: scoped reports (digest, audit) must keep the typed safe
// projection, preserve only authorized link context (in-scope peers), and
// never surface hidden peer identities.
func TestIssueScopedReportsPreserveAuthorizedLinkContext(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside peer", nil)
	newScopedTokens(t, env, project, root)
	coordinatorHeaders := map[string]string{"Authorization": "Bearer coordinator-token"}

	// Compound link edit mixing an authorized edge (child→root) with a hidden
	// peer (child→outside) in one issue.links_changed event.
	resp, body := envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID,
		"issues/"+child.ShortID),
		map[string]any{"actor": "coordinator", "links_delta": map[string]any{
			"add_related": []string{outside.ShortID},
		}}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	resp, body = envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID,
		"issues/"+child.ShortID),
		map[string]any{"actor": "coordinator", "links_delta": map[string]any{
			"add_related": []string{root.ShortID},
		}}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	// Create with an authorized initial link (related→child) and a hidden one
	// (blocked-by outside) folded into the issue.created payload.
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues"),
		map[string]any{
			"actor": "coordinator", "title": "Linked work",
			"links": []map[string]any{
				{"type": "parent", "to_ref": root.ShortID},
				{"type": "related", "to_ref": child.ShortID},
				{"type": "blocks", "to_ref": outside.ShortID, "incoming": true},
			},
		}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var created struct {
		Issue db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(body, &created))

	// Close it so the audit projection has a parent context to preserve.
	resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID,
		"issues/"+created.Issue.ShortID+"/actions/close"),
		map[string]any{
			"actor": "coordinator", "reason": "done",
			"message":  "Work landed and was independently verified end to end.",
			"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
		}, coordinatorHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	resp, body = envDoRaw(t, env, http.MethodGet,
		scopedProjectPath(project.ID, "digest")+"?since="+since, nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.NotContains(t, string(body), outside.UID, "digest must hide hidden peer UIDs")
	require.NotContains(t, string(body), outside.ShortID, "digest must hide hidden peer refs")
	// Authorized link changes survive: the compound edit's child→root edge
	// and the created issue's related→child link must both be counted.
	require.Contains(t, string(body), "related:"+root.ShortID,
		"digest must preserve authorized issue.links_changed edges")
	require.Contains(t, string(body), "related:"+child.ShortID,
		"digest must preserve authorized issue.created initial links")

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/audit/closes?project_id="+strconv.FormatInt(project.ID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.NotContains(t, string(body), outside.UID)
	require.NotContains(t, string(body), outside.ShortID)
	var audit struct {
		Rows []struct {
			Issue         string   `json:"issue"`
			Parent        string   `json:"parent"`
			ParentUID     string   `json:"parent_uid"`
			Reason        string   `json:"reason"`
			EvidenceTypes []string `json:"evidence_types"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(body, &audit))
	var closedRow *struct {
		Issue         string   `json:"issue"`
		Parent        string   `json:"parent"`
		ParentUID     string   `json:"parent_uid"`
		Reason        string   `json:"reason"`
		EvidenceTypes []string `json:"evidence_types"`
	}
	for index := range audit.Rows {
		if audit.Rows[index].Issue == created.Issue.ShortID {
			closedRow = &audit.Rows[index]
		}
	}
	require.NotNil(t, closedRow, fmt.Sprintf("missing audit row for %s", created.Issue.ShortID))
	require.Equal(t, "done", closedRow.Reason)
	require.Equal(t, root.ShortID, closedRow.Parent,
		"audit must preserve the authorized (in-scope) parent context")
	require.Equal(t, root.UID, closedRow.ParentUID)
	require.Equal(t, []string{"test"}, closedRow.EvidenceTypes)
}
