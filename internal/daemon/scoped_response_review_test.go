package daemon_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestScopedCommentResponseRejectsPostCommitOutsideContent(t *testing.T) {
	var wrapped *commentResponseRaceStore
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(),
		func(cfg *daemon.ServerConfig) {
			wrapped = &commentResponseRaceStore{Storage: cfg.DB}
			cfg.DB = wrapped
		})
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree,
			ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	outsideBody := "Content added after the child left the delegated subtree"
	wrapped.afterComment = func() error {
		_, err := env.DB.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{
			IssueID: child.ID, Actor: "coordinator", RemoveParent: &root.ID, Body: &outsideBody,
		})
		return err
	}

	_, body := envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+child.ShortID+"/comments",
		map[string]string{"body": "Authorized comment"},
		map[string]string{"Authorization": "Bearer worker-token"})

	require.NotContains(t, string(body), outsideBody)
	comments, err := env.DB.CommentsByIssue(t.Context(), child.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1, "the authorized comment committed before scope changed")
}

type commentResponseRaceStore struct {
	db.Storage
	afterComment func() error
}

func TestScopedCollectionsRejectOutsideContentDuringHydration(t *testing.T) {
	for _, route := range []string{"issues", "labels", "graph"} {
		t.Run(route, func(t *testing.T) {
			var wrapped *collectionResponseRaceStore
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(),
				func(cfg *daemon.ServerConfig) {
					wrapped = &collectionResponseRaceStore{Storage: cfg.DB}
					cfg.DB = wrapped
				})
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
			child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
			_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
				PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
				Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree,
					ProjectUID: project.UID, RootIssueUID: root.UID},
				ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
			})
			require.NoError(t, err)
			outsideContent := "content-added-outside-scope"
			wrapped.beforeHydration = func() error {
				_, err := env.DB.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{
					IssueID: child.ID, Actor: "coordinator", RemoveParent: &root.ID, Body: &outsideContent,
				})
				if err != nil {
					return err
				}
				_, err = env.DB.AddLabel(t.Context(), child.ID, outsideContent, "coordinator")
				return err
			}
			path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/" + route
			if route == "graph" {
				wrapped.issueID = child.ID
				path = "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + root.ShortID + "/graph"
			}
			resp, body := envDoRaw(t, env, http.MethodGet, path, nil,
				map[string]string{"Authorization": "Bearer worker-token"})
			require.NotContains(t, string(body), outsideContent)
			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
		})
	}
}

type collectionResponseRaceStore struct {
	db.Storage
	issueID         int64
	beforeHydration func() error
}

func (s *collectionResponseRaceStore) LabelsByIssues(ctx context.Context, projectID int64, ids []int64) (map[int64][]string, error) {
	if s.issueID == 0 && s.beforeHydration != nil {
		fn := s.beforeHydration
		s.beforeHydration = nil
		if err := fn(); err != nil {
			return nil, err
		}
	}
	return s.Storage.LabelsByIssues(ctx, projectID, ids)
}

func (s *collectionResponseRaceStore) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	if id == s.issueID && s.beforeHydration != nil {
		fn := s.beforeHydration
		s.beforeHydration = nil
		if err := fn(); err != nil {
			return db.Issue{}, err
		}
	}
	return s.Storage.IssueByID(ctx, id)
}

func (s *commentResponseRaceStore) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	comment, event, err := s.Storage.CreateComment(ctx, p)
	if err == nil {
		err = s.afterComment()
	}
	return comment, event, err
}
