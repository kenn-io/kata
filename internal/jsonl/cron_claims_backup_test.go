package jsonl_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func claimBackupFixture(t *testing.T) (db.Storage, db.CronOccurrenceClaim) {
	t.Helper()
	s := openExportTestDB(t)
	p, err := s.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	j, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	c := db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: j.UID, DefinitionEventUID: j.DefinitionEventUID, RunUID: "00000000000000000000000003", HolderInstanceUID: s.InstanceUID(), Actor: "worker", OccurrenceKey: "2030-01-02T03:04:05Z"}
	return s, c
}

func TestCronClaimsOwnerBackupRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, in := claimBackupFixture(t)
			out, err := source.ClaimCronOccurrence(t.Context(), in)
			require.NoError(t, err)
			var data bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), source, &data, jsonl.ExportOptions{IncludeDeleted: true}))
			var target db.Storage
			if backend == "postgres" {
				dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
				t.Cleanup(cleanup)
				target, err = pgstore.Open(t.Context(), dsn)
			} else {
				target, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(data.Bytes()), target))
			got, err := target.CronOccurrenceClaim(t.Context(), in.ProjectID, in.RunUID)
			require.NoError(t, err)
			require.Equal(t, out.Claim, got)
			replay, err := target.ClaimCronOccurrence(t.Context(), in)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			in.RunUID = "00000000000000000000000004"
			_, err = target.ClaimCronOccurrence(t.Context(), in)
			require.ErrorIs(t, err, db.ErrCronOccurrenceClaimed)
		})
	}
}

func TestCronClaimsProjectImportCannotBecomeAuthority(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{true: "retained", false: "before_first_claim"}[claimed], func(t *testing.T) {
			source, in := claimBackupFixture(t)
			if claimed {
				_, err := source.ClaimCronOccurrence(t.Context(), in)
				require.NoError(t, err)
			}
			var data bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), source, &data, jsonl.ExportOptions{ProjectID: in.ProjectID, IncludeDeleted: true}))
			for _, mode := range []string{"ordinary", "standalone", "new-instance", "merge"} {
				t.Run(mode, func(t *testing.T) {
					target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "copy.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = target.Close() })
					opts := jsonl.ImportOptions{AsStandalone: mode == "standalone", NewInstance: mode == "new-instance", MergeProject: mode == "merge"}
					err = jsonl.ImportWithOptions(t.Context(), bytes.NewReader(data.Bytes()), target, opts)

					require.NoError(t, err)
					_, err = target.CronOccurrenceClaim(t.Context(), in.ProjectID, in.RunUID)
					require.ErrorIs(t, err, db.ErrNotFound, "project export omits claims")
					in.RunUID = "00000000000000000000000005"
					_, err = target.ClaimCronOccurrence(t.Context(), in)
					require.ErrorIs(t, err, db.ErrCronClaimPending, "copy must not grant original namespace")
				})
			}
		})
	}
}

func TestCronClaimsOwnerNewInstanceCannotRegrant(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_first_claim", true: "retained"}[claimed], func(t *testing.T) {
			source, in := claimBackupFixture(t)
			if claimed {
				_, err := source.ClaimCronOccurrence(t.Context(), in)
				require.NoError(t, err)
			}
			var data bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), source, &data, jsonl.ExportOptions{IncludeDeleted: true}))
			for _, standalone := range []bool{false, true} {
				t.Run(map[bool]string{false: "new-instance", true: "standalone"}[standalone], func(t *testing.T) {
					target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "copy.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = target.Close() })
					require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(data.Bytes()), target, jsonl.ImportOptions{NewInstance: true, AsStandalone: standalone}))
					in.RunUID = "00000000000000000000000006"
					_, err = target.ClaimCronOccurrence(t.Context(), in)
					require.ErrorIs(t, err, db.ErrCronClaimPending)
				})
			}
		})
	}
}

func TestCronClaimsBackupConflictsRollbackReplacement(t *testing.T) {
	source, in := claimBackupFixture(t)
	_, err := source.ClaimCronOccurrence(t.Context(), in)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &data, jsonl.ExportOptions{IncludeDeleted: true}))
	lines := bytes.Split(bytes.TrimSpace(data.Bytes()), []byte("\n"))
	var altered bytes.Buffer
	found := false
	for _, line := range lines {
		altered.Write(line)
		altered.WriteByte('\n')
		if bytes.Contains(line, []byte(`"kind":"cron_occurrence_claim"`)) {
			altered.Write(line)
			altered.WriteByte('\n')
			found = true
		}
	}
	require.True(t, found)
	target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	retained, err := target.CreateProject(t.Context(), "retained-project")
	require.NoError(t, err)
	err = jsonl.Import(t.Context(), bytes.NewReader(altered.Bytes()), target)
	require.Error(t, err)
	_, err = target.ProjectByUID(t.Context(), retained.UID)
	require.NoError(t, err, "failed restore preserves original target data")
}
