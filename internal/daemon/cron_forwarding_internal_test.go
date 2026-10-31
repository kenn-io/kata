package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Storage BeginTx owns request-fence application; pre-send admission adds only
// the live project check in that same transaction.
func TestCronForwardAdmissionAppliesFenceOnce(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	fences, projects := 0, 0
	ctx := db.WithTransactionFence(t.Context(), func(context.Context, db.Transaction) error { fences++; return nil })
	ctx = db.WithProjectAccessTransactionCheck(ctx, func(context.Context, db.Transaction, string) error { projects++; return nil })
	require.NoError(t, cronForwardAdmission(ctx, store, project.ID, project.UID))
	require.Equal(t, 1, fences)
	require.Equal(t, 1, projects)
}
