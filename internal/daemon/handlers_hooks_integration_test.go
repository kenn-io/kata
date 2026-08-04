package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

// recordingSink captures every Enqueue for assertion. Lives only in tests;
// production paths use the real *hooks.Dispatcher.
type recordingSink struct {
	mu     sync.Mutex
	events []db.Event
}

func (r *recordingSink) Enqueue(evt db.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
}

func (r *recordingSink) snapshot() []db.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]db.Event, len(r.events))
	copy(out, r.events)
	return out
}

var _ hooks.Sink = (*recordingSink)(nil)

// TestHooks_ProjectAndIssueCreateEnqueueCommittedEvents exercises both create
// handlers and verifies hook delivery carries their persisted events.
func TestHooks_ProjectAndIssueCreateEnqueueCommittedEvents(t *testing.T) {
	sink := &recordingSink{}
	h := newServerWithGitWorkspace(t, "", withHooksSink(sink))
	ts := h.ts.(*httptest.Server)

	resp, body := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "example-project", "actor": "user-a",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var initialized struct {
		Project db.Project `json:"project"`
	}
	require.NoError(t, json.Unmarshal(body, &initialized))
	captured := sink.snapshot()
	require.Len(t, captured, 1, "project creation should enqueue its committed event")
	assert.Equal(t, "project.created", captured[0].Type)
	assert.Equal(t, "user-a", captured[0].Actor)
	assert.Equal(t, initialized.Project.ID, captured[0].ProjectID)
	assert.NotZero(t, captured[0].ID)

	resp, body = postJSON(t, ts, issuesURL(initialized.Project.ID),
		map[string]any{"actor": "agent-1", "title": "first", "body": "details"})
	require.Equal(t, 200, resp.StatusCode, string(body))

	captured = sink.snapshot()
	require.Len(t, captured, 2, "project and issue events should both be enqueued")
	assert.Equal(t, "issue.created", captured[1].Type)
	assert.Equal(t, "agent-1", captured[1].Actor)
	assert.NotZero(t, captured[1].ID, "captured event should carry the persisted row id")
}

func TestInitProjectDeliversCommittedEventWhenConfigWriteFails(t *testing.T) {
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()
	h := newServerWithGitWorkspace(t, "", withHooksSink(sink), withBroadcaster(broadcaster))
	require.NoError(t, os.Mkdir(filepath.Join(h.dir, ".kata.toml"), 0o700))

	response, body := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", map[string]any{
		"name": "example-project", "start_path": h.dir, "actor": "user-a",
	})
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))

	msg := receiveMsg(t, subscription.Ch, time.Second, "project create before config failure")
	require.Equal(t, "event", msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, "project.created", msg.Event.Type)
	assert.Equal(t, "user-a", msg.Event.Actor)
	captured := sink.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, *msg.Event, captured[0])

	project, err := h.DB().ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	persisted, err := h.DB().EventsAfter(t.Context(), db.EventsAfterParams{
		ProjectID: project.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	assert.Equal(t, *msg.Event, persisted[0])
}

func TestInitProjectBroadcastsResetWhenAliasFailureCleansUpCreatedProject(t *testing.T) {
	database := openTestDB(t)
	store := &attachAliasFailureStore{Storage: database.db}
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()
	server := startTestServer(t, daemon.ServerConfig{
		DB: store, StartedAt: database.now, Hooks: sink, Broadcaster: broadcaster,
	})

	response, body := postJSON(t, server, "/api/v1/projects", map[string]any{
		"name": "example-project", "actor": "user-a",
		"alias": map[string]any{"identity": "example-project", "kind": "git"},
	})
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))

	msg := receiveMsg(t, subscription.Ch, time.Second, "project cleanup reset")
	assert.Equal(t, "reset", msg.Kind)
	assert.Positive(t, msg.ResetID)
	assert.Empty(t, sink.snapshot())
	_, err := database.db.ProjectByName(t.Context(), "example-project")
	assert.ErrorIs(t, err, db.ErrNotFound)
	resetID, err := database.db.PurgeResetCheck(t.Context(), 0, 0)
	require.NoError(t, err)
	assert.Equal(t, resetID, msg.ResetID)
}

func TestRenameProjectDeliversCommittedEventWhenAliasReadFails(t *testing.T) {
	database := openTestDB(t)
	project, err := database.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	store := &projectAliasesFailureStore{Storage: database.db}
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()
	server := startTestServer(t, daemon.ServerConfig{
		DB: store, StartedAt: database.now, Hooks: sink, Broadcaster: broadcaster,
	})

	response, body := patchJSON(t, server, fmt.Sprintf("/api/v1/projects/%d", project.ID), map[string]any{
		"name": "renamed-project", "actor": "user-a",
	})
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))

	msg := receiveMsg(t, subscription.Ch, time.Second, "project rename before alias read failure")
	require.Equal(t, "event", msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, "project.renamed", msg.Event.Type)
	assert.Equal(t, "user-a", msg.Event.Actor)
	captured := sink.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, *msg.Event, captured[0])

	persisted, err := database.db.EventsAfter(t.Context(), db.EventsAfterParams{
		ProjectID: project.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, persisted, 2)
	assert.Equal(t, *msg.Event, persisted[1])
}

func TestFederationReplicaDeliversCreatedEventWhenCredentialWriteFails(t *testing.T) {
	database := openTestDB(t)
	credentials := newReplicaCredentialStore()
	credentials.storeErr = errors.New("forced credential write failure")
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()
	server := startTestServer(t, daemon.ServerConfig{
		DB: database.db, StartedAt: database.now, FederationCredentials: credentials,
		Hooks: sink, Broadcaster: broadcaster,
	})

	response, body := postJSON(t, server, "/api/v1/federation/replicas", map[string]any{
		"hub_url": "https://hub.example", "hub_project_id": 42,
		"hub_project_uid": replicaHubProjectUID, "project_name": "example-project",
		"replay_horizon_event_id": 9, "actor": "user-a", "token": "example-token",
		"capabilities": "pull", "push_enabled": false,
	})
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))

	msg := receiveMsg(t, subscription.Ch, time.Second, "replica create before credential failure")
	require.Equal(t, "event", msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, "project.created", msg.Event.Type)
	assert.Equal(t, "user-a", msg.Event.Actor)
	captured := sink.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, *msg.Event, captured[0])

	project, err := database.db.ProjectByUID(t.Context(), replicaHubProjectUID)
	require.NoError(t, err)
	persisted, err := database.db.EventsAfter(t.Context(), db.EventsAfterParams{
		ProjectID: project.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	assert.Equal(t, *msg.Event, persisted[0])
}

type attachAliasFailureStore struct {
	db.Storage
}

func (*attachAliasFailureStore) AttachAlias(
	context.Context, int64, string, string,
) (db.ProjectAlias, error) {
	return db.ProjectAlias{}, errors.New("forced alias attachment failure")
}

type projectAliasesFailureStore struct {
	db.Storage
}

func (*projectAliasesFailureStore) ProjectAliases(context.Context, int64) ([]db.ProjectAlias, error) {
	return nil, errors.New("forced alias read failure")
}

func TestRecurrenceMutationsBroadcastAndEnqueueCommittedEvents(t *testing.T) {
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	h, projectID := bootstrapProject(t, withHooksSink(sink), withBroadcaster(broadcaster))
	ts := h.ts.(*httptest.Server)
	subscription := broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectID})
	defer subscription.Unsub()
	path := fmt.Sprintf("/api/v1/projects/%d/recurrences", projectID)

	response, body := postJSON(t, ts, path, map[string]any{
		"actor": "user-a", "rrule": "FREQ=WEEKLY", "dtstart": "2026-08-03",
		"timezone": "UTC", "template": map[string]any{"title": "Weekly review"},
	})
	require.Equal(t, http.StatusCreated, response.StatusCode, string(body))
	var created struct {
		Recurrence db.Recurrence `json:"recurrence"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	createdMsg := receiveMsg(t, subscription.Ch, time.Second, "recurrence create")
	require.NotNil(t, createdMsg.Event)
	assert.Equal(t, "recurrence.created", createdMsg.Event.Type)

	recurrencePath := path + "/" + created.Recurrence.UID
	response, body = doReq(t, ts, http.MethodPatch, recurrencePath,
		map[string]any{"actor": "user-a", "template": map[string]any{"title": "Updated review"}},
		map[string]string{"If-Match": `"rev-1"`})
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	updatedMsg := receiveMsg(t, subscription.Ch, time.Second, "recurrence update")
	require.NotNil(t, updatedMsg.Event)
	assert.Equal(t, "recurrence.updated", updatedMsg.Event.Type)

	response, body = doReq(t, ts, http.MethodDelete, recurrencePath+"?actor=user-a", nil,
		map[string]string{"If-Match": `"rev-2"`})
	require.Equal(t, http.StatusNoContent, response.StatusCode, string(body))
	deletedMsg := receiveMsg(t, subscription.Ch, time.Second, "recurrence delete")
	require.NotNil(t, deletedMsg.Event)
	assert.Equal(t, "recurrence.deleted", deletedMsg.Event.Type)

	events := sink.snapshot()
	require.Len(t, events, 3)
	assert.Equal(t, []string{"recurrence.created", "recurrence.updated", "recurrence.deleted"},
		[]string{events[0].Type, events[1].Type, events[2].Type})
	assert.Equal(t, []int64{createdMsg.Event.ID, updatedMsg.Event.ID, deletedMsg.Event.ID},
		[]int64{events[0].ID, events[1].ID, events[2].ID})
}
