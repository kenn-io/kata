package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

// Admission freshness is measured after transaction/lock waits, rather than at
// HTTP arrival. Virtual time makes the 30-second boundary deterministic.
func TestCronRequestClockAfterTransactionWait(t *testing.T) {
	for _, collected := range []bool{false, true} {
		name := "unseen"
		if collected {
			name = "collected"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tx, _, _ := referenceWorkFixture(t, 0)
				_, err := tx.ExecContext(t.Context(), `CREATE TABLE federation_bindings(project_id INTEGER,role TEXT)`)
				require.NoError(t, err)
				requested := time.Now().Add(-29 * time.Second)
				id, err := uid.FromTime(requested)
				require.NoError(t, err)
				if collected {
					retained, err := json.Marshal(map[string]any{CronRequestMetadataPrefix + id: map[string]any{"kind": "cancel", "run_uid": "01M40000000000000000000004", "requested_by": "operator", "requested_at": requested, "expires_at": requested.Add(time.Minute)}})
					require.NoError(t, err)
					_, err = tx.ExecContext(t.Context(), `UPDATE projects SET metadata=$1 WHERE id=1`, string(retained))
					require.NoError(t, err)
				}
				engine := CronSQL{
					Transact: func(_ context.Context, fn func(*sql.Tx) error) error {
						if collected {
							time.Sleep(61 * time.Second)
							_, e := tx.ExecContext(t.Context(), `UPDATE projects SET metadata='{}' WHERE id=1`)
							require.NoError(t, e, "another transaction collected the retained request during the wait")
						} else {
							time.Sleep(2 * time.Second)
						}
						return fn(tx)
					},
					WriteGate: func(context.Context, *sql.Tx, int64) error { return nil },
				}
				out, err := engine.Request(t.Context(), RequestCronIn{ProjectID: 1, UID: id, Kind: "cancel", TargetUID: "01M40000000000000000000004", Actor: "operator"})
				require.ErrorIs(t, err, ErrCronRequestExpired, "expired or collected identity must fail before target lookup")
				require.Nil(t, out.Event)
			})
		})
	}
}
