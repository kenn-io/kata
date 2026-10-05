package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type projectAuthorizationProbeStore struct {
	db.Storage
	revisionReads     int
	projectLists      int
	policyReads       int
	anonymousUIDReads int
}

func (s *projectAuthorizationProbeStore) ProjectAccessRevision(context.Context) (int64, error) {
	s.revisionReads++
	return 1, nil
}

func (s *projectAuthorizationProbeStore) ListProjects(context.Context) ([]db.Project, error) {
	s.projectLists++
	return []db.Project{{UID: "spoke-project-a"}, {UID: "spoke-project-b"}}, nil
}

func (s *projectAuthorizationProbeStore) ProjectAccessPolicy(_ context.Context, projectUID string) (db.ProjectAccessPolicy, error) {
	s.policyReads++
	return db.ProjectAccessPolicy{ProjectUID: projectUID, Visibility: "all"}, nil
}

func (s *projectAuthorizationProbeStore) AnonymousAccessibleProjectUIDs(ctx context.Context) ([]string, error) {
	s.anonymousUIDReads++
	query, ok := s.Storage.(interface {
		AnonymousAccessibleProjectUIDs(context.Context) ([]string, error)
	})
	if !ok {
		return nil, nil
	}
	return query.AnonymousAccessibleProjectUIDs(ctx)
}

func TestProjectAuthorizationSkipsProjectQueriesForHealthProbes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	for _, path := range []string{pathPing, pathHealth} {
		t.Run(path, func(t *testing.T) {
			store := &projectAuthorizationProbeStore{}
			handler := withProjectAuthorization(store, true, false, selfAuthenticatedRouteMatcher{}, srv.noProjectDataRoutes, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

			if response.Code != http.StatusNoContent {
				t.Fatalf("probe status = %d, want %d", response.Code, http.StatusNoContent)
			}
			if store.revisionReads != 0 || store.projectLists != 0 || store.policyReads != 0 {
				t.Fatalf("probe project queries = revision:%d list:%d policy:%d; want none", store.revisionReads, store.projectLists, store.policyReads)
			}
		})
	}
}

func TestProjectAuthorizationStillScopesOrdinaryAnonymousAPIRoutes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	store := &projectAuthorizationProbeStore{}
	handler := withProjectAuthorization(store, true, false, selfAuthenticatedRouteMatcher{}, srv.noProjectDataRoutes, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

	if response.Code != http.StatusNoContent {
		t.Fatalf("route status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if store.revisionReads != 2 || store.projectLists != 0 || store.policyReads != 0 || store.anonymousUIDReads != 1 {
		t.Fatalf("ordinary route project queries = revision:%d list:%d policy:%d anonymous_uid:%d; want revision:2, list:0, policy:0, anonymous_uid:1", store.revisionReads, store.projectLists, store.policyReads, store.anonymousUIDReads)
	}
}

func TestProjectAuthorizationDefersSelfAuthenticatedFederationRoutes(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	probe := &projectAuthorizationProbeStore{Storage: store}
	srv := NewServer(ServerConfig{DB: probe})
	t.Cleanup(func() { _ = srv.Close() })
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer invalid-federation-token")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("invalid federation credential status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if probe.revisionReads != 2 || probe.projectLists != 0 || probe.policyReads != 0 || probe.anonymousUIDReads != 1 {
		t.Fatalf("self-authenticated route project queries = revision:%d list:%d policy:%d anonymous_uid:%d; want revision:2, list:0, policy:0, anonymous_uid:1", probe.revisionReads, probe.projectLists, probe.policyReads, probe.anonymousUIDReads)
	}
}

func TestProjectAuthorizationOwnerSelectsTeamsRestrictedProjectForTransportRoute(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	team, _, err := store.CreateTeam(t.Context(), "operator-team", "owner")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := store.SetTeamMembership(t.Context(), team.UID, "member", true, "owner"); err != nil {
		t.Fatalf("add project member: %v", err)
	}
	if _, _, err := store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
		ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
	}, "owner"); err != nil {
		t.Fatalf("restrict project to its team: %v", err)
	}
	srv := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { _ = srv.Close() })
	handler := withProjectAuthorization(
		store, false, false, srv.authPolicy.SelfAuthenticatedRoutes, srv.noProjectDataRoutes,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := authorizeProjectTarget(r.Context(), project.UID); err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/abc4/lease/actions/acquire"
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request = request.WithContext(WithPrincipal(request.Context(), Principal{Kind: PrincipalStaticToken}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("owner transport project selection status = %d, want %d", response.Code, http.StatusNoContent)
	}
}
