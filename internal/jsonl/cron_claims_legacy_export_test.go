package jsonl

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestCronClaimsVersionAwareExport(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	p, err := s.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	j, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	in := db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: j.UID, DefinitionEventUID: j.DefinitionEventUID, RunUID: "00000000000000000000000003", HolderInstanceUID: s.InstanceUID(), Actor: "worker", OccurrenceKey: "2030-01-02T03:04:05Z"}
	out, err := s.ClaimCronOccurrence(t.Context(), in)
	require.NoError(t, err)
	for _, project := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "project"}[project], func(t *testing.T) {
			opts := ExportOptions{IncludeDeleted: true}
			if project {
				opts.ProjectID = p.ID
			}
			var data bytes.Buffer
			require.NoError(t, exportSnapshot(t.Context(), s, &data, opts))
			target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, Import(t.Context(), bytes.NewReader(data.Bytes()), target))
			if project {
				in.RunUID = "00000000000000000000000004"
				_, err = target.ClaimCronOccurrence(t.Context(), in)
				require.ErrorIs(t, err, db.ErrCronClaimPending)
			} else {
				got, err := target.CronOccurrenceClaim(t.Context(), p.ID, in.RunUID)
				require.NoError(t, err)
				require.Equal(t, out.Claim, got)
			}
		})
	}
}
