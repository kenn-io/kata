package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db/dbtest/faultsql"
)

func TestCronReferenceOperationalErrorChains(t *testing.T) {
	job, version, projectUID := "01M40000000000000000000001", "01M40000000000000000000002", "01M40000000000000000000003"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := CronRun{UID: "01M40000000000000000000004", ProjectID: 1, JobUID: &job, DefinitionEventUID: &version, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	body, err := json.Marshal(NewCronRunObservation(run, projectUID))
	require.NoError(t, err)
	event := RemoteEvent{Type: "cron.run.observed", EventUID: "01M40000000000000000000005", ProjectUID: projectUID, OriginInstanceUID: "01M40000000000000000000006", HLCPhysicalMS: at.UnixMilli(), Payload: body}
	_, err = parseCronRunObservation(FoldEvent{Type: event.Type, UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, Payload: event.Payload})
	require.NoError(t, err, "faults must exercise a valid observation")
	callers := []struct {
		name string
		call func(*sql.Tx) error
	}{
		{"replay", func(tx *sql.Tx) error { return NewCronReplayValidator(tx, false).Validate(t.Context(), 1, event) }},
		{"materialize", func(tx *sql.Tx) error {
			return materializeCronRuns(t.Context(), tx, 1, projectUID, FoldProjection{CronRuns: map[string]FoldCronRun{run.UID: {CronRun: run, ProjectUID: projectUID}}}, NewCronReplayValidator(tx, false))
		}},
		{"local", func(tx *sql.Tx) error {
			return NewCronReplayValidator(tx, false).validateLocalReference(t.Context(), Project{ID: 1, UID: projectUID}, "job", job, version)
		}},
	}
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			for _, fault := range []error{&pgconn.PgError{Code: "40001"}, context.Canceled, context.DeadlineExceeded, errors.New("read unavailable")} {
				requireEveryQueryFaultPropagates(t, caller.call, fault)
			}
			for _, scan := range []bool{false, true} {
				script := &faultsql.Script{Query: func(int64) (driver.Rows, error) {
					rows := &faultsql.Rows{Names: []string{"project", "event"}, Err: context.Canceled}
					if scan {
						rows.Err = nil
						rows.Values = [][]driver.Value{{"invalid integer", version}}
					}
					return rows, nil
				}}
				pool := faultsql.Open(t, script)
				tx, err := pool.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				err = caller.call(tx)
				if scan {
					require.ErrorContains(t, err, "invalid syntax")
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
				require.False(t, errors.Is(err, ErrFederationIngestValidation))
				require.False(t, errors.Is(err, cron.ErrInvalid))
				require.NoError(t, tx.Rollback())
			}
		})
	}
}

// requireEveryQueryFaultPropagates fails each query the caller issues in turn,
// stopping once the caller finishes without reaching the injected query. Each
// operational fault must reach the caller unclassified as invalid input.
func requireEveryQueryFaultPropagates(t *testing.T, call func(*sql.Tx) error, fault error) {
	t.Helper()
	const maxQueries = 100
	for query := int64(1); ; query++ {
		require.LessOrEqual(t, query, int64(maxQueries), "caller kept querying after %d faults", maxQueries)
		injected := false
		script := &faultsql.Script{Query: func(count int64) (driver.Rows, error) {
			if count == query {
				injected = true
				return nil, fault
			}
			return &faultsql.Rows{}, nil
		}}
		pool := faultsql.Open(t, script)
		tx, err := pool.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		err = call(tx)
		require.NoError(t, tx.Rollback())
		if !injected {
			require.Greater(t, query, int64(1), "caller issued no queries")
			return
		}
		require.ErrorIs(t, err, fault, "fault at query %d", query)
		require.False(t, errors.Is(err, ErrFederationIngestValidation))
		require.False(t, errors.Is(err, cron.ErrInvalid))
	}
}
