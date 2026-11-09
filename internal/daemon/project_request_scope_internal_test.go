package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/projectscope"
)

func TestRequestProjectScopeSurvivesClaimAuthority(t *testing.T) {
	decision := &ProjectAccessDecision{ProjectUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, fixedUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}}
	ctx := context.WithValue(t.Context(), projectAccessContextKey{}, decision)
	ctx = claimRequestContext(ctx, claimPrincipal{OwnerAuthority: true})
	projects, restricted := db.AuthorizedProjects(ctx)
	require.True(t, restricted)
	require.Equal(t, []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, projects)
	require.Equal(t, projects, decision.ProjectUIDs)
	require.True(t, decision.owner, "validated owner authority remains intact")
}

func TestRequestProjectScopeIntersectsFederationAuthority(t *testing.T) {
	decision := &ProjectAccessDecision{ProjectUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, fixedUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}}
	ctx := context.WithValue(t.Context(), projectAccessContextKey{}, decision)
	ctx = withFederationProjectAuthority(ctx, federationAuthorization{projectUID: "01HBBBBBBBBBBBBBBBBBBBBBBB"})
	projects, restricted := db.AuthorizedProjects(ctx)
	require.True(t, restricted)
	require.Empty(t, projects)
	require.Empty(t, decision.ProjectUIDs)
	require.Error(t, authorizeProjectTarget(ctx, "01HBBBBBBBBBBBBBBBBBBBBBBB"))
}

type requestScopeRevisionStore struct {
	db.Storage
}

func (*requestScopeRevisionStore) ProjectAccessRevision(context.Context) (int64, error) {
	return 2, nil
}

func TestRequestProjectScopeRevalidatesOwnerStream(t *testing.T) {
	store := &requestScopeRevisionStore{}
	decision := &ProjectAccessDecision{ProjectUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, fixedUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, owner: true, PolicyRevision: 1, store: store}
	ctx := context.WithValue(t.Context(), projectAccessContextKey{}, decision)
	revalidated, err := revalidateEventStream(ctx, store, true)
	require.NoError(t, err)
	require.True(t, db.ProjectAttributionVisible(revalidated, "01HAAAAAAAAAAAAAAAAAAAAAAA"), "the original project remains admitted")
	require.False(t, db.ProjectAttributionVisible(revalidated, "01HBBBBBBBBBBBBBBBBBBBBBBB"), "owner stream revalidation must exclude projects outside its explicit scope")
}

type requestScopeLookupStore struct {
	db.Storage
	err error
}

func (s *requestScopeLookupStore) ProjectByID(context.Context, int64) (db.Project, error) {
	return db.Project{}, s.err
}

// Contract: a missing numeric project is not found, while a storage failure
// stays an internal error instead of masquerading as a missing project.
func TestRequestProjectScopeLookupErrors(t *testing.T) {
	header := http.Header{}
	header.Set(projectscope.Header, `["id:7"]`)
	for _, tc := range []struct {
		err    error
		status int
	}{{db.ErrNotFound, http.StatusNotFound}, {errors.New("database unavailable"), http.StatusInternalServerError}} {
		_, apiErr := requestProjectScope(t.Context(), &requestScopeLookupStore{err: tc.err}, header)
		require.NotNil(t, apiErr)
		require.Equal(t, tc.status, apiErr.Status)
	}
}
