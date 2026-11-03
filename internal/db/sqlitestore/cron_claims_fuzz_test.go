package sqlitestore_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A model retains a winner per key and an immutable identity per successful UID.
func FuzzCronOccurrenceClaimIdentity(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 3, 1, 4, 0})
	f.Add([]byte{0, 4, 1, 5, 2, 6, 3, 7})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 32 {
			ops = ops[:32]
		}
		s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "claims.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		p, err := s.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
		require.NoError(t, err)
		j, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
		require.NoError(t, err)
		byKey := map[string]string{}
		byRun := map[string]string{}
		for _, op := range ops {
			key := fmt.Sprintf("occurrence-%d", op%4)
			run := fmt.Sprintf("000000000000000000000000%02d", (op/4)%8)
			out, e := s.ClaimCronOccurrence(t.Context(), db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: j.UID, RunUID: run, DefinitionEventUID: j.DefinitionEventUID, OccurrenceKey: key, HolderInstanceUID: s.InstanceUID(), Actor: "worker"})
			switch {
			case byRun[run] != "" && byRun[run] != key:
				require.True(t, errors.Is(e, db.ErrCronClaimIdentity))
			case byRun[run] == key:
				require.NoError(t, e)
				require.True(t, out.Replayed)
			case byKey[key] != "":
				require.ErrorIs(t, e, db.ErrCronOccurrenceClaimed)
				require.Equal(t, byKey[key], out.Claim.RunUID)
			default:
				require.NoError(t, e)
				require.False(t, out.Replayed)
				byRun[run] = key
				byKey[key] = run
			}
		}
	})
}
