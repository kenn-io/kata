package sqlitestore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	katauid "go.kenn.io/kata/internal/uid"
)

// BenchmarkPendingFederationPushSnapshotRun grows the uninterrupted snapshot
// run from a bulk import. Only reading the pending events is timed.
func BenchmarkPendingFederationPushSnapshotRun(b *testing.B) {
	for _, count := range []int{1000, 3000, 10_000} {
		b.Run(fmt.Sprintf("snapshots=%d", count), func(b *testing.B) {
			d, project, _ := incrementalTestStore(b)
			ctx := b.Context()
			var afterID int64
			require.NoError(b, d.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&afterID))
			tx, err := d.BeginTx(ctx, nil)
			require.NoError(b, err)
			defer func() { _ = tx.Rollback() }()
			for i := range count {
				issueUID, err := katauid.New()
				require.NoError(b, err)
				_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid, project_id, short_id, title, author)
					VALUES(?, ?, ?, 'imported issue', 'tester')`, issueUID, project.ID, strings.ToLower(issueUID[18:]))
				require.NoError(b, err)
				eventUID, err := katauid.New()
				require.NoError(b, err)
				payload := fmt.Sprintf(`{"uid":%q,"title":"imported issue","author":"tester","status":"open"}`, issueUID)
				_, err = tx.ExecContext(ctx, `INSERT INTO events(
					uid, origin_instance_uid, project_id, project_name, issue_uid,
					type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
					VALUES(?, ?, ?, ?, ?, 'issue.snapshot', 'tester', ?, ?, 0, ?)`,
					eventUID, d.InstanceUID(), project.ID, project.Name, issueUID, payload, i+1, strings.Repeat("a", 64))
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())
			b.ResetTimer()
			for range b.N {
				events, err := d.PendingFederationPushEvents(ctx, project.ID, d.InstanceUID(), afterID, 1000)
				b.StopTimer()
				require.NoError(b, err)
				require.Len(b, events, count)
				b.StartTimer()
			}
		})
	}
}
