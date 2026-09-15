package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type blockingResponseWriter struct {
	header  http.Header
	body    bytes.Buffer
	entered chan struct{}
	release chan struct{}
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }
func (w *blockingResponseWriter) WriteHeader(int)     {}
func (w *blockingResponseWriter) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.body.Write(body)
}
func (w *blockingResponseWriter) Flush() {}

func TestScopedEventPayloadPreservesTypedEmitterFields(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		payload   string
		want      map[string]any
	}{
		{
			name:      "metadata diff and revision",
			eventType: "issue.metadata_updated",
			payload:   `{"diff":{"work.attention":"ok"},"revision_new":4,"updated_at":"2026-09-15T01:02:03Z"}`,
			want: map[string]any{
				"diff":         map[string]any{"work.attention": "ok"},
				"revision_new": float64(4),
				"updated_at":   "2026-09-15T01:02:03Z",
			},
		},
		{
			name:      "comment edit timestamp",
			eventType: "issue.comment_edited",
			payload:   `{"comment_uid":"comment-1","body":"updated","edited_at":"2026-09-15T01:02:03Z"}`,
			want: map[string]any{
				"comment_uid": "comment-1",
				"body":        "updated",
				"edited_at":   "2026-09-15T01:02:03Z",
			},
		},
		{
			name:      "cleared priority",
			eventType: "issue.priority_cleared",
			payload:   `{"old_priority":2,"updated_at":"2026-09-15T01:02:03Z"}`,
			want: map[string]any{
				"old_priority": float64(2),
				"updated_at":   "2026-09-15T01:02:03Z",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected, projectable := scopedEventPayload(db.Event{Type: tt.eventType, Payload: tt.payload})
			require.True(t, projectable, "known event type must project")
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(projected), &got))
			require.Equal(t, tt.want, got)
		})
	}
}

func TestProjectIssueScopedEventRequiresRecordedProject(t *testing.T) {
	issueID := int64(7)
	event := db.Event{
		ID:         11,
		ProjectUID: "outside-project-uid",
		IssueID:    &issueID,
		Type:       "issue.updated",
		Payload:    `{"title":"Old project title"}`,
	}

	_, ok := projectIssueScopedEvent(event, map[int64]struct{}{issueID: {}}, "granted-project-uid")

	require.False(t, ok)
}

func TestScopedEventPayloadRejectsUnknownTypeWithEmptyPayload(t *testing.T) {
	_, ok := scopedEventPayload(db.Event{Type: "issue.future_mutation"})
	require.False(t, ok)
}

// TestScopedEventPayloadProjectsLifecycleAndMoveEvents pins the typed
// projection for the ladder verbs and cross-project moves. The emitters
// (sqlitestore queries_delete.go / queries_move.go and their pgstore twins)
// write fixed payload shapes: soft_deleted carries deleted_at; restored
// carries restored_at + updated_at; moved carries the issue, both project
// UIDs, both short ids, and updated_at. The scoped projection keeps the
// timestamps plus the arrival identity inside the client's own project
// and strips the source project's UID and short id — coordinates of a
// project the scope cannot see.
func TestScopedEventPayloadProjectsLifecycleAndMoveEvents(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		payload   string
		want      string
	}{
		{
			name:      "soft delete timestamp",
			eventType: "issue.soft_deleted",
			payload:   `{"deleted_at":"2026-09-15T01:02:03Z"}`,
			want:      `{"deleted_at":"2026-09-15T01:02:03Z"}`,
		},
		{
			name:      "restore timestamps",
			eventType: "issue.restored",
			payload:   `{"restored_at":"2026-09-15T01:02:03Z","updated_at":"2026-09-15T01:02:04Z"}`,
			want:      `{"restored_at":"2026-09-15T01:02:03Z","updated_at":"2026-09-15T01:02:04Z"}`,
		},
		{
			name:      "move keeps arrival identity, strips source project",
			eventType: "issue.moved",
			payload: `{"issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVX","from_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVA",` +
				`"from_short_id":"ab12","to_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVB","to_short_id":"cd34",` +
				`"updated_at":"2026-09-15T01:02:03Z"}`,
			want: `{"to_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVB","to_short_id":"cd34",` +
				`"updated_at":"2026-09-15T01:02:03Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected, ok := scopedEventPayload(db.Event{Type: tt.eventType, Payload: tt.payload})
			require.True(t, ok, "lifecycle/move event type must project")
			require.JSONEq(t, tt.want, projected)
		})
	}
}

// TestProjectIssueScopedEventAcceptsLifecycleAndMoveEvents covers the
// envelope-level projection: an in-scope lifecycle or move event must be
// delivered (not fail closed into a scoped reset) once its payload type is
// known, and the redaction of ContentHash / OriginInstanceUID still applies.
func TestProjectIssueScopedEventAcceptsLifecycleAndMoveEvents(t *testing.T) {
	issueID := int64(9)
	tests := []struct {
		eventType string
		payload   string
	}{
		{eventType: "issue.soft_deleted", payload: `{"deleted_at":"2026-09-15T01:02:03Z"}`},
		{eventType: "issue.restored", payload: `{"restored_at":"2026-09-15T01:02:03Z","updated_at":"2026-09-15T01:02:03Z"}`},
		{eventType: "issue.moved", payload: `{"from_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVA","to_project_uid":"granted-project-uid","to_short_id":"cd34","updated_at":"2026-09-15T01:02:03Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			event := db.Event{
				ID: 21, ProjectUID: "granted-project-uid", IssueID: &issueID,
				Type: tt.eventType, Payload: tt.payload,
				ContentHash: "sha-secret", OriginInstanceUID: "instance-uid",
			}
			projected, ok := projectIssueScopedEvent(event, map[int64]struct{}{issueID: {}}, "granted-project-uid")
			require.True(t, ok, "in-scope lifecycle/move event must stay visible")
			require.Empty(t, projected.ContentHash)
			require.Empty(t, projected.OriginInstanceUID)
			require.NotContains(t, projected.Payload, "from_project_uid")
		})
	}
}

func TestScopedLinksChangedReportPayloadRequiresPairedAuthorizedUIDs(t *testing.T) {
	payload := `{"related_added":["hidden-short-id"],"related_added_uids":[]}`
	projected := scopedLinksChangedReportPayload(payload, map[string]struct{}{"allowed-uid": {}})
	require.Empty(t, projected)
	require.NotContains(t, projected, "hidden-short-id")
}

type maxJumpStorage struct {
	db.Storage
	maxCalls atomic.Int64
}

type issueByIDCountingStorage struct {
	db.Storage
	issueByIDCalls atomic.Int64
}

func (s *issueByIDCountingStorage) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	s.issueByIDCalls.Add(1)
	return s.Storage.IssueByID(ctx, id)
}

func TestReadVisibleEventsDoesNotHydrateUIDsWithoutCompoundLinkEvents(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	_ = createScopedAuthIssue(t, store, project.ID, "Child", &root)
	expiresAt := time.Now().UTC().Add(time.Hour)
	token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	wrapped := &issueByIDCountingStorage{Storage: store}
	ctx := WithPrincipal(t.Context(), principalFromAPIToken(token))

	_, _, _, err = readVisibleEvents(ctx, wrapped, 0, project.ID, 0, 100)

	require.NoError(t, err)
	require.Zero(t, wrapped.issueByIDCalls.Load(),
		"ordinary event pages should not perform one UID lookup per allowed issue")
}

func (s *maxJumpStorage) MaxEventID(_ context.Context) (int64, error) {
	s.maxCalls.Add(1)
	return 999999, nil
}

func TestReadVisibleEventsNeverJumpsToSeparateMaxRead(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	wrapped := &maxJumpStorage{Storage: store}
	ctx := WithPrincipal(t.Context(), Principal{
		Kind: PrincipalDBToken, Actor: token.Actor, TokenID: token.ID,
		Scope: token.Scope, ExpiresAt: token.ExpiresAt,
	})

	_, cursor, resetTo, err := readVisibleEvents(ctx, wrapped, 0, project.ID, 0, 100)

	require.NoError(t, err)
	require.Zero(t, resetTo)
	require.NotEqual(t, int64(999999), cursor)
	require.Zero(t, wrapped.maxCalls.Load())
}

// purgedWindowStorage simulates the SSE live-drain race where a purge
// commits after the live phase's PurgeResetCheck but before the scoped scan:
// every durable event in (AfterID, ThroughID] is already deleted when
// EventsAfter runs, so the window reads back empty.
type purgedWindowStorage struct {
	db.Storage
	windowReads atomic.Int64
}

func (s *purgedWindowStorage) EventsAfter(ctx context.Context, p db.EventsAfterParams) ([]db.Event, error) {
	if p.ThroughID > 0 && p.AfterID < p.ThroughID {
		s.windowReads.Add(1)
		return nil, nil
	}
	return s.Storage.EventsAfter(ctx, p)
}

func TestReadVisibleEventsScopedEmptyWindowAdvancesCursorToThroughID(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	through, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, through, int64(0))
	lastSent := through - 1
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	wrapped := &purgedWindowStorage{Storage: store}

	rows, scannedTo, resetTo, err := readVisibleEvents(ctx, wrapped, lastSent, project.ID, through, sseLiveBatch)

	require.NoError(t, err)
	require.Zero(t, resetTo)
	require.Empty(t, rows)
	require.Equal(t, int64(1), wrapped.windowReads.Load(),
		"an emptied window must be settled by a single batch read")
	require.Equal(t, through, scannedTo,
		"an emptied scoped window must advance the cursor to throughID so the live drain loop terminates")
}

func TestLivePhaseScopedPurgedWindowTerminatesDrain(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	through, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, through, int64(0))
	lastSent := through - 1

	ctx, cancel := context.WithCancel(withScopedAuthorizationTestPrincipal(t, store, project, root))
	defer cancel()
	wrapped := &purgedWindowStorage{Storage: store}
	response := httptest.NewRecorder()
	messages := make(chan StreamMsg, 1)
	messages <- NewEventMsg(project.ID, db.Event{ID: through})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLivePhase(ctx, livePhaseDeps{
			w: response, flusher: response, cfg: ServerConfig{DB: wrapped}, ch: messages,
		}, project.ID, lastSent)
	}()

	require.Eventually(t, func() bool { return wrapped.windowReads.Load() >= 1 },
		5*time.Second, time.Millisecond, "live phase must consume the wakeup")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(1), wrapped.windowReads.Load(),
		"a purged wakeup window must not be re-queried in an unbounded loop")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("live phase did not return after context cancellation")
	}
}

func TestScopedEventFrameDoesNotBlockWritesWhileClientStalls(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	child := createScopedAuthIssue(t, store, project.ID, "Child", &root)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	writer := &blockingResponseWriter{
		header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{}),
	}
	done := make(chan bool, 1)
	go func() {
		done <- writeRevalidatedEventFrame(ctx, store, writer, writer, db.Event{
			ID: 17, ProjectUID: project.UID, IssueID: &child.ID, Type: "issue.updated",
		})
	}()
	<-writer.entered

	mutationDone := make(chan error, 1)
	go func() {
		_, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{
			IssueID: child.ID, Author: "coordinator", Body: "Progress continues",
		})
		mutationDone <- err
	}()
	defer close(writer.release)
	select {
	case err := <-mutationDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("stalled event client blocked an independent mutation")
	}
	t.Cleanup(func() { require.True(t, <-done) })
}

func TestScopedPollBoundsHiddenEventScanAndResumesCursor(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	hidden := createScopedAuthIssue(t, store, project.ID, "Outside", nil)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	afterID, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	for range 1001 {
		_, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{
			IssueID: hidden.ID, Author: "coordinator", Body: "Hidden progress",
		})
		require.NoError(t, err)
	}
	_, visible, err := store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: root.ID, Author: "coordinator", Body: "Visible progress",
	})
	require.NoError(t, err)
	first, err := doPollEvents(ctx, ServerConfig{DB: store}, afterID, api.OptionalInt{IsSet: true, Value: 1}, project.ID)
	require.NoError(t, err)
	require.False(t, first.Body.ResetRequired)
	require.Empty(t, first.Body.Events, "a page must stop scanning hidden rows within its work budget")
	require.Greater(t, first.Body.NextAfterID, afterID)
	require.Less(t, first.Body.NextAfterID, visible.ID)
	second, err := doPollEvents(ctx, ServerConfig{DB: store}, first.Body.NextAfterID, api.OptionalInt{IsSet: true, Value: 1}, project.ID)
	require.NoError(t, err)
	require.Len(t, second.Body.Events, 1)
	require.Equal(t, visible.ID, second.Body.Events[0].EventID)
	require.Equal(t, visible.ID, second.Body.NextAfterID)
}

type reparentBeforeEventsStorage struct {
	db.Storage
	childID int64
	rootID  int64
}

func (s *reparentBeforeEventsStorage) EventsAfter(ctx context.Context, params db.EventsAfterParams) ([]db.Event, error) {
	_, err := s.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: s.childID, Actor: "coordinator", RemoveParent: &s.rootID,
	})
	if err != nil {
		return nil, err
	}
	_, _, err = s.CreateComment(ctx, db.CreateCommentParams{
		IssueID: s.childID, Author: "coordinator", Body: "Outside progress",
	})
	if err != nil {
		return nil, err
	}
	return s.Storage.EventsAfter(ctx, params)
}

func TestScopedPollAuthorizesMembershipAfterReadingEvents(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	child := createScopedAuthIssue(t, store, project.ID, "Child", &root)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	afterID, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	wrapped := &reparentBeforeEventsStorage{Storage: store, childID: child.ID, rootID: root.ID}
	response, err := doPollEvents(ctx, ServerConfig{DB: wrapped}, afterID, api.OptionalInt{}, project.ID)
	require.NoError(t, err)
	require.Empty(t, response.Body.Events, "events written after a child leaves the subtree must stay hidden")
	require.True(t, response.Body.ResetRequired)
}
