package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Move payloads carry a projection snapshot that older peers cannot apply.
// The existing feature negotiation must reject them before delivering data.
func TestFederationMoveRequiresFeature(t *testing.T) {
	require.Equal(t, "issue_move_v1", db.EventRequiredFeatures("issue.moved"))
	require.ErrorIs(t, db.RequireEventFeatures(db.CronEventFeature, db.EventRequiredFeatures("issue.moved")), db.ErrUnsupportedEventFeatures)
}
