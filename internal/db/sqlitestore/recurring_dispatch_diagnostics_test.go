package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Even a no-op gate is only an observation if its transaction finishes. A
// transaction failure must not be mislabeled as an ordinary missing-job gate.
func TestRecurringDecisionOnTransactionFailure(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	p, err := s.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	a := s.cronSQL()
	transact := a.Transact
	failure := errors.New("transaction finalization failed")
	a.Transact = func(ctx context.Context, run func(*sql.Tx) error) error {
		return transact(ctx, func(tx *sql.Tx) error {
			if err := run(tx); err != nil {
				return err
			}
			return failure
		})
	}
	out, err := a.DispatchRecurring(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	require.ErrorIs(t, err, failure)
	require.Equal(t, cron.DispatchError, out.Reason)
	require.Empty(t, out.Events)
}
