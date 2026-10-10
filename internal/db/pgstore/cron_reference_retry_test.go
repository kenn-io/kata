package pgstore

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest/faultsql"
)

func TestCronReferenceReadReceivesWholeTransactionRetry(t *testing.T) {
	job, version, projectUID := "01M40000000000000000000001", "01M40000000000000000000002", "01M40000000000000000000003"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := db.CronRun{UID: "01M40000000000000000000004", ProjectID: 1, JobUID: &job, DefinitionEventUID: &version, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	body, err := json.Marshal(db.NewCronRunObservation(run, projectUID))
	require.NoError(t, err)
	event := db.RemoteEvent{Type: "cron.run.observed", EventUID: "01M40000000000000000000005", ProjectUID: projectUID, OriginInstanceUID: "01M40000000000000000000006", HLCPhysicalMS: at.UnixMilli(), Payload: body}
	for _, state := range []string{"40001", "40P01", "55P03"} {
		t.Run(state, func(t *testing.T) {
			script := &faultsql.Script{Query: func(count int64) (driver.Rows, error) {
				if count == 1 {
					return nil, &pgconn.PgError{Code: state}
				}
				return &faultsql.Rows{}, nil
			}}
			store := &Store{DB: faultsql.Open(t, script)}
			attempts := 0
			err := store.withSerializableTx(t.Context(), func(tx *sql.Tx) error {
				attempts++
				if attempts == 2 {
					require.Equal(t, int64(1), script.Rollbacks.Load(), "failed transaction rolls back before retry")
				}
				err := db.NewCronReplayValidator(tx, true).Validate(t.Context(), 1, event)
				if attempts == 1 {
					assert.True(t, IsTransient(err), "the index error retains its SQLState")
				}
				return err
			})
			assert.NoError(t, err)
			assert.Equal(t, 2, attempts)
			assert.Equal(t, int64(2), script.Begins.Load())
			assert.Equal(t, int64(1), script.Commits.Load())
		})
	}
}
