package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Complete hydration before delivering the move, so only release-time access
// revision revalidation can prevent the admitted response escaping its scope.
type projectAccessAfterClaimReadStore struct {
	db.Storage
	issueUID  string
	afterRead func() error
}

func (s *projectAccessAfterClaimReadStore) ListPendingClaimRequestsForIssue(ctx context.Context, projectID int64, issueUID string, limit int) ([]db.PendingClaimRequest, error) {
	pending, err := s.Storage.ListPendingClaimRequestsForIssue(ctx, projectID, issueUID, limit)
	if err == nil && issueUID == s.issueUID && s.afterRead != nil {
		after := s.afterRead
		s.afterRead = nil
		err = after()
	}
	return pending, err
}

func TestProjectAccessRechecksCompletedReadAfterFederationMoveReplay(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "replay-hidden")
		_, err := store.SetTeamMembership(t.Context(), f.team.UID, "other-member", true, "admin")
		require.NoError(t, err)
		for _, project := range []db.Project{f.private, hidden} {
			_, err := store.EnableProjectFederation(t.Context(), project.ID, "member")
			require.NoError(t, err)
		}
		baselineStatus, _, baselineRaw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.private.ID, f.issue.ShortID), "member", nil, nil)
		require.Equal(t, http.StatusOK, baselineStatus, string(baselineRaw))

		origin, err := uid.New()
		require.NoError(t, err)
		eventUID, err := uid.New()
		require.NoError(t, err)
		at := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
		payload, err := json.Marshal(map[string]any{"issue_uid": f.issue.UID, "from_project_uid": f.private.UID, "from_short_id": f.issue.ShortID, "to_project_uid": hidden.UID, "to_short_id": f.issue.ShortID, "updated_at": at.Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": f.issue.UID, "short_id": f.issue.ShortID, "title": f.issue.Title, "body": f.issue.Body, "author": "member", "status": "open"}})
		require.NoError(t, err)
		event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: hidden.UID, ProjectName: hidden.Name, IssueUID: &f.issue.UID, Type: "issue.moved", Actor: "other-member", Payload: jsontext.Value(payload), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		replayed := false
		var replayErr error
		wrapped := &projectAccessAfterClaimReadStore{Storage: store, issueUID: f.issue.UID}
		wrapped.afterRead = func() error {
			_, err := store.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: hidden.ID, SpokeInstanceUID: origin, BoundActor: "other-member", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
			replayErr = err
			replayed = err == nil
			return err
		}
		f = projectAccessFixtureWithStorage(t, f, wrapped)
		status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.private.ID, f.issue.ShortID), "member", nil, nil)
		require.NoError(t, replayErr, "replay callback")
		require.True(t, replayed, "the incoming move commits after hydration and before response release: status=%d body=%s", status, raw)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), projectAccessCanary)
	})
}

func TestProjectAccessRetainedMoveDestinationIsPrivate(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true, Actor: "member"})
		require.NoError(t, err)
		destination, err := uid.New()
		require.NoError(t, err)
		origin, err := uid.New()
		require.NoError(t, err)
		eventUID, err := uid.New()
		require.NoError(t, err)
		at := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
		payload, err := json.Marshal(map[string]any{"issue_uid": f.issue.UID, "from_project_uid": f.private.UID, "from_short_id": f.issue.ShortID, "to_project_uid": destination, "to_short_id": f.issue.ShortID, "updated_at": at.Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": f.issue.UID, "short_id": f.issue.ShortID, "title": f.issue.Title, "body": f.issue.Body, "author": "member", "status": "open"}})
		require.NoError(t, err)
		move := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: destination, ProjectName: "destination", IssueUID: &f.issue.UID, Type: "issue.moved", Actor: "member", Payload: jsontext.Value(payload), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
		move.ContentHash, err = db.RemoteEventContentHash(move)
		require.NoError(t, err)
		_, err = store.InsertRemoteEvents(ctx, f.private.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(ctx, f.private.ID))
		project, err := store.ProjectByUID(ctx, destination)
		require.NoError(t, err)
		require.NotNil(t, project.DeletedAt)
		status, _, raw := f.request(t, http.MethodGet, "/api/v1/projects?include=archived", "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NotContains(t, string(raw), project.Name)
		path := fmt.Sprintf("/api/v1/projects/%d", project.ID)
		status, _, raw = f.request(t, http.MethodPost, path+"/restore?actor=nonmember", "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		ownerServer := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token"}})
		t.Cleanup(func() { require.NoError(t, ownerServer.Close()) })
		ownerHTTP := httptest.NewServer(ownerServer.Handler())
		t.Cleanup(ownerHTTP.Close)
		owner := f
		owner.server = ownerHTTP
		status, _, raw = owner.request(t, http.MethodPost, path+"/restore?actor=admin", "admin", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		issue, err := store.IssueByUID(ctx, f.issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		issuePath := fmt.Sprintf("%s/issues/%s", path, issue.ShortID)
		status, _, raw = f.request(t, http.MethodGet, issuePath, "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
	})
}
