package federation

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationMoveHistoryProvesSnapshotPreservesAcceptedCommentIdentity(t *testing.T) {
	createdAt := "2026-01-01T00:00:00Z"
	history := []db.FoldEvent{
		{UID: "created-event", ProjectUID: "hub-project", IssueUID: "issue", Type: "issue.created", Actor: "old-actor", Payload: jsontext.Value(`{"uid":"issue","author":"old-actor","title":"task","body":"","status":"open","metadata":{},"created_at":"2026-01-01T00:00:00Z"}`)},
		{UID: "comment-event", ProjectUID: "hub-project", IssueUID: "issue", Type: "issue.commented", Actor: "old-actor", Payload: jsontext.Value(`{"comment_uid":"known-comment","author":"old-actor","body":"accepted","created_at":"2026-01-01T00:00:00Z"}`)},
	}
	accepted := db.IssueDocumentPayload{UID: "issue", Author: "old-actor", Comments: []db.IssueDocumentComment{{CommentUID: "known-comment", Author: "old-actor", Body: "accepted", CreatedAt: createdAt}}}
	currentActorComment := db.IssueDocumentComment{CommentUID: "new-comment", Author: "new-actor", Body: "new", CreatedAt: createdAt}
	accepted.Comments = append(accepted.Comments, currentActorComment)
	payload, err := json.Marshal(accepted)
	require.NoError(t, err)
	require.True(t, db.FederationMoveHistoryProvesSnapshot("issue", "hub-project", history, string(payload), "new-actor"))

	accepted.Comments[1].Author = "old-actor"
	payload, err = json.Marshal(accepted)
	require.NoError(t, err)
	require.False(t, db.FederationMoveHistoryProvesSnapshot("issue", "hub-project", history, string(payload), "new-actor"))

	accepted.Comments[1] = db.IssueDocumentComment{CommentUID: "known-comment", Author: "new-actor", Body: "accepted", CreatedAt: createdAt}
	payload, err = json.Marshal(accepted)
	require.NoError(t, err)
	require.False(t, db.FederationMoveHistoryProvesSnapshot("issue", "hub-project", history, string(payload), "new-actor"))
}

func TestRejoinMoveWithOlderDiscussion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published bool
		edited    bool
		pushOnly  bool
		restored  bool
		offline   bool
		unknown   bool
	}{
		{name: "unpublished task", unknown: true},
		{name: "unpublished"},
		{name: "published", published: true},
		{name: "unpublished comment edit", edited: true},
		{name: "published comment edit", published: true, edited: true},
		{name: "push only", published: true, pushOnly: true},
		{name: "former instance", published: true, restored: true},
		{name: "hub history offline", published: true, offline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			var discussion db.Comment
			if !tc.unknown {
				require.NoError(t, spoke.sync(t, 0))
				discussion, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "discussion"})
				require.NoError(t, err)
			}
			if tc.edited {
				require.NoError(t, spoke.sync(t, 0))
				_, _, _, err = spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: discussion.UID, Actor: "old-actor", Body: "edited discussion"})
				require.NoError(t, err)
			}
			if tc.published && !tc.unknown {
				require.NoError(t, spoke.sync(t, 0))
			}
			if tc.restored {
				records, err := dbtest.CollectImportRecords(ctx, spoke.env.DB, db.ExportFilter{IncludeDeleted: true})
				require.NoError(t, err)
				restored := testenv.New(t)
				require.NoError(t, restored.DB.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
				spoke.env = restored
			}
			capabilities := "push,pull,claim"
			if tc.pushOnly {
				capabilities = "push,claim"
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, capabilities)
			_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[1].ID)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull,claim", Actor: "new-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[1].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			spoke.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "new-actor"}
			if tc.pushOnly {
				require.NoError(t, pushRejoin(t, spoke))
			} else {
				require.NoError(t, spoke.sync(t, 0))
			}
			if tc.offline {
				base, err := url.Parse(hub.URL)
				require.NoError(t, err)
				reverse := httputil.NewSingleHostReverseProxy(base)
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Get("issue_uid") != "" {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					reverse.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				for _, project := range projects {
					binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
					require.NoError(t, err)
					binding.HubURL = proxy.URL
					_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
					require.NoError(t, err)
					credential := spoke.credentials[project.UID]
					credential.HubURL = proxy.URL
					spoke.credentials[project.UID] = credential
				}
			}
			t.Setenv("KATA_HOME", spoke.env.Home)
			for _, project := range projects {
				credential := spoke.credentials[project.UID]
				credential.Capabilities = "push,pull,claim"
				if project.UID == projects[0].UID {
					credential.Capabilities = capabilities
				}
				require.NoError(t, config.WriteFederationCredential(project.UID, credential))
			}
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			body := []byte(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q}`, projects[1].UID))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.UID), bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
			resp, err := spoke.env.HTTP.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tc.offline {
				require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
				return
			}
			if tc.unknown || tc.pushOnly {
				require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &envelope))
				require.Equal(t, "federation_offline", envelope.Error.Code)
				return
			}
			if !tc.published {
				require.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &envelope))
				require.Equal(t, "federated_move_unsupported", envelope.Error.Code)
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
		})
	}
}

func TestRejoinMoveRejectsDiscussionChangedDuringHubCheck(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	var validationSpy *moveSnapshotValidationSpy
	spoke := newMoveDeliverySpokeWithOptions(t, hub, projects, []testenv.Option{
		func(cfg *daemon.ServerConfig) {
			validationSpy = &moveSnapshotValidationSpy{Storage: cfg.DB}
			cfg.DB = validationSpy
		},
	}, "old-actor", "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	acceptedComment, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "accepted discussion"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[1].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull,claim", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[1].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "new-actor"}

	base, err := url.Parse(hub.URL)
	require.NoError(t, err)
	started := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	var startedOnce sync.Once
	resumeHistory := func() { resumeOnce.Do(func() { close(resume) }) }
	reverse := httputil.NewSingleHostReverseProxy(base)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Get("issue_uid") != "" {
			startedOnce.Do(func() { close(started) })
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(resumeHistory)
	for i, project := range spoke.projects {
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		binding.HubURL = proxy.URL
		_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		credential := spoke.credentials[projects[i].UID]
		credential.HubURL = proxy.URL
		credential.Capabilities = "push,pull,claim"
		spoke.credentials[projects[i].UID] = credential
		require.NoError(t, config.WriteFederationCredential(projects[i].UID, credential))
	}

	issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	body := []byte(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q}`, projects[1].UID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.UID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
	type response struct {
		status int
		body   []byte
		err    error
	}
	result := make(chan response, 1)
	go func() {
		resp, err := spoke.env.HTTP.Do(req)
		if err != nil {
			result <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		result <- response{status: resp.StatusCode, body: raw, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		got := <-result
		t.Fatalf("move did not request hub issue history: status=%d body=%s err=%v", got.status, got.body, got.err)
	}
	_, _, _, err = spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: acceptedComment.UID, Actor: "new-actor", Body: "arrived during history check"})
	require.NoError(t, err)
	resumeHistory()
	got := <-result
	require.NoError(t, got.err)
	require.Equal(t, 1, validationSpy.calls)
	var movedSnapshot db.IssueDocumentPayload
	require.NoError(t, json.Unmarshal([]byte(validationSpy.payload), &movedSnapshot))
	require.Len(t, movedSnapshot.Comments, 1)
	require.Equal(t, "old-actor", movedSnapshot.Comments[0].Author)
	require.Equal(t, "arrived during history check", movedSnapshot.Comments[0].Body)
	acceptedEvents, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[0].ID, IssueUID: issue.UID, Limit: 1000})
	require.NoError(t, err)
	history := make([]db.FoldEvent, len(acceptedEvents))
	for i, event := range acceptedEvents {
		history[i] = db.FoldEvent{
			UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID,
			IssueUID: issue.UID, Type: event.Type, Actor: event.Actor,
			HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
			CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano), Payload: []byte(event.Payload),
		}
	}
	require.False(t, db.FederationMoveHistoryProvesSnapshot(issue.UID, projects[0].UID, history, validationSpy.payload, "new-actor"))
	require.Equal(t, http.StatusConflict, got.status, string(got.body))
	var envelope api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(got.body, &envelope))
	require.Equal(t, "federated_move_unsupported", envelope.Error.Code)
	stored, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[0].ID, stored.ProjectID)

	other, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "independent destination task", Author: "new-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 1))
	_, err = hub.DB.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}
