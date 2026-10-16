package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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
	reads int
}

func (s *requestScopeRevisionStore) ProjectAccessRevision(context.Context) (int64, error) {
	s.reads++
	return 2, nil
}

func TestRequestProjectScopeRevalidatesOwnerStream(t *testing.T) {
	store := &requestScopeRevisionStore{}
	decision := &ProjectAccessDecision{ProjectUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, fixedUIDs: []string{"01HAAAAAAAAAAAAAAAAAAAAAAA"}, owner: true, PolicyRevision: 1, store: store}
	ctx := context.WithValue(t.Context(), projectAccessContextKey{}, decision)
	_, err := revalidateEventStream(ctx, store, true)
	require.NoError(t, err)
	require.Positive(t, store.reads, "limited owner streams must revalidate before delivering frames")
	require.Equal(t, int64(2), decision.PolicyRevision)
}
