package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv/sqlcount"
)

func TestQuietFederationPollTransactions(t *testing.T) {
	t.Cleanup(pgstore.CountStatementsForTest())
	dsn, cleanup := conformanceTemplate.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.OpenWithConfig(t.Context(), dsn, conformanceConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	quiet, err := s.CreateProject(t.Context(), "quiet")
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(t.Context(), quiet.ID, "tester")
	require.NoError(t, err)
	cursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	var count sqlcount.Counter
	ctx := count.Context(t.Context())
	in := db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 100}
	page, err := s.ReadFederation(ctx, in)
	require.NoError(t, err)
	require.Empty(t, page.Events)
	require.Zero(t, count.Begins())
}
