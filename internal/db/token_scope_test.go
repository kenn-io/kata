package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestActiveAPITokenGrantExpiresAtBoundary(t *testing.T) {
	expiresAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	token := APIToken{ID: 1, Actor: "worker", ExpiresAt: &expiresAt, Scope: &APITokenScope{Kind: APITokenScopeIssueSubtree, ProjectUID: "project", RootIssueUID: "root"}}
	require.True(t, ActiveAPITokenGrantMatches(token, token, expiresAt.Add(-time.Nanosecond)))
	require.False(t, ActiveAPITokenGrantMatches(token, token, expiresAt))
}
