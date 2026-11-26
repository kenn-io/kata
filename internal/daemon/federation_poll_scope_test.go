package daemon_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// federationPollScopeStore counts project-list reads and can change project
// policy while a poll is checking its federation token or after it read its
// events, after the request was admitted and before its response is released.
type federationPollScopeStore struct {
	db.Storage
	anonymousLists   atomic.Int64
	actorLists       atomic.Int64
	duringTokenCheck func() error
	afterRead        func() error
}

func (s *federationPollScopeStore) AnonymousAccessibleProjectUIDs(ctx context.Context) ([]string, error) {
	s.anonymousLists.Add(1)
	return s.Storage.AnonymousAccessibleProjectUIDs(ctx)
}

func (s *federationPollScopeStore) AccessibleProjectUIDs(ctx context.Context, actor string) ([]string, error) {
	s.actorLists.Add(1)
	return s.Storage.AccessibleProjectUIDs(ctx, actor)
}

func (s *federationPollScopeStore) ReadFederation(ctx context.Context, params db.FederationReadParams) (db.FederationReadResult, error) {
	result, err := s.Storage.ReadFederation(ctx, params)
	if after := s.afterRead; err == nil && after != nil {
		s.afterRead = nil
		if err := after(); err != nil {
			return db.FederationReadResult{}, err
		}
	}
	return result, err
}

func (s *federationPollScopeStore) AuthorizeFederationToken(ctx context.Context, token string, projectID int64, capability string) (db.FederationEnrollment, error) {
	if during := s.duringTokenCheck; during != nil {
		s.duringTokenCheck = nil
		if err := during(); err != nil {
			return db.FederationEnrollment{}, err
		}
	}
	return s.Storage.AuthorizeFederationToken(ctx, token, projectID, capability)
}

type federationPollScopeFixture struct {
	store          *federationPollScopeStore
	target, other  db.Project
	token          string
	restrictPolicy func(db.Project) func() error
	poll           func(token string) *httptest.ResponseRecorder
	pollProject    func(projectID int64, token string) *httptest.ResponseRecorder
}

func newFederationPollScopeFixture(t *testing.T, store db.Storage) federationPollScopeFixture {
	t.Helper()
	ctx := t.Context()
	target, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, target.ID, "admin")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "other-team", "admin")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ // #nosec G101 -- synthetic test enrollment token, not a real secret.
		Token:            "poll-scope-enrollment",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &target.ID,
		Capabilities:     "pull",
		Actor:            "example-actor",
	})
	require.NoError(t, err)
	scope := &federationPollScopeStore{Storage: store}
	server := daemon.NewServer(daemon.ServerConfig{
		DB:        scope,
		StartedAt: time.Now().UTC(),
		Auth:      config.AuthConfig{Token: "hub-admin-token", TrustPrivateNetwork: true},
	})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	pollProject := func(projectID int64, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(ctx, http.MethodGet,
			projectPath(projectID)+"/federation/events?after_id=0&limit=10", nil)
		request.RemoteAddr = "100.64.0.9:41000"
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP("100.64.0.5"), Port: 7777}))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	return federationPollScopeFixture{
		store: scope, target: target, other: other, token: enrollment.Token,
		restrictPolicy: func(project db.Project) func() error {
			return func() error {
				_, _, err := store.SetProjectAccessPolicy(context.WithoutCancel(ctx), db.ProjectAccessPolicy{
					ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
				}, "admin")
				return err
			}
		},
		poll:        func(token string) *httptest.ResponseRecorder { return pollProject(target.ID, token) },
		pollProject: pollProject,
	}
}

// A refused poll keeps its refusal when an unrelated project's policy changes
// while the request is in flight.
func TestFederationPollRefusalIgnoresUnrelatedPolicyChanges(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newFederationPollScopeFixture(t, store)
		fixture.store.duringTokenCheck = fixture.restrictPolicy(fixture.other)
		response := fixture.poll("not-an-enrollment-token")
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "auth_invalid")
		require.Zero(t, fixture.store.anonymousLists.Load(), "release recheck read the anonymous project list")
	})
}

// A federation poll is authorized against its own project. Neither its
// admission nor the release recheck after its policy revision moves reads a
// hub-wide project list.
func TestFederationPollReleaseRechecksOnlyItsProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newFederationPollScopeFixture(t, store)
		fixture.store.afterRead = fixture.restrictPolicy(fixture.other)
		response := fixture.poll(fixture.token)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Zero(t, fixture.store.actorLists.Load(), "poll read the actor's project list")
		require.Zero(t, fixture.store.anonymousLists.Load(), "poll read the anonymous project list")
	})
}

// A refused poll whose own project leaves anonymous visibility while the
// request is in flight is hidden, as before.
func TestFederationPollRefusalFollowsItsProjectPolicy(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newFederationPollScopeFixture(t, store)
		fixture.store.duringTokenCheck = fixture.restrictPolicy(fixture.target)
		response := fixture.poll("not-an-enrollment-token")
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		require.Zero(t, fixture.store.anonymousLists.Load(), "release recheck read the anonymous project list")
	})
}

// A refused poll for a project that does not exist keeps its refusal scoped
// to that project when policy changes while the request is in flight.
func TestFederationPollRefusalForMissingProjectStaysScoped(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newFederationPollScopeFixture(t, store)
		fixture.store.duringTokenCheck = fixture.restrictPolicy(fixture.other)
		response := fixture.pollProject(fixture.target.ID+1000, fixture.token)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		require.Zero(t, fixture.store.anonymousLists.Load(), "release recheck read the anonymous project list")
	})
}
