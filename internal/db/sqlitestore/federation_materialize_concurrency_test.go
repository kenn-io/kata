package sqlitestore_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A spoke materializes pulled events while another process keeps committing.
// Materialization must take the writer lock before it reads the event log:
// a deferred transaction that reads first and writes later fails with
// SQLITE_BUSY_SNAPSHOT (517) whenever another connection commits in between,
// and under steady local writes every retry loses the same race. The store's
// write queue already orders writers within one handle, so the competing
// writer uses a second handle, where only the lock prevents the race.
// The race is timing-based: the test reliably catches a missing lock or an
// event-log read ahead of it, but not a single short read squeezed in first.
func TestMaterializeFederatedProjectSurvivesConcurrentLocalWrites(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "local-writer")
	require.NoError(t, err)
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	const issues = 300
	for i := range issues {
		uid := newTestUID(t)
		snapshot := fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"issue %d","body":"","author":"alice",
			"status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`, uid, shortID(uid), i)
		inserted, err := d.InsertRemoteEvent(ctx, p.ID,
			remoteEvent(t, p.UID, "hub", &uid, nil, "issue.snapshot", "remote-agent", int64(100+i), snapshot))
		require.NoError(t, err)
		require.True(t, inserted)
	}

	peer, err := sqlitestore.Open(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	writerCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		for writerCtx.Err() == nil {
			_, _ = peer.DB.ExecContext(writerCtx, `UPDATE projects SET name = name WHERE id = ?`, other.ID)
		}
	})

	// One attempt each, no retry: under the old read-then-write transaction
	// most attempts fail; with the writer lock taken first none may.
	var failures []error
	for range 20 {
		if err := sqlitestore.MaterializeFederatedProjectOnceForTest(ctx, d, p.ID); err != nil {
			failures = append(failures, err)
		}
	}
	stop()
	wg.Wait()
	require.Empty(t, failures, "materialization lost the writer race: %v", failures)
}
