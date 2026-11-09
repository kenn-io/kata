package sqlitestore

import (
	"context"
	"fmt"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	katauid "go.kenn.io/kata/internal/uid"
)

// BenchmarkFederationIngestWriterInterval reports both total ingest time and
// the interval from the project lock through commit. History preparation
// is intentionally included in ns/op and excluded from writer-ns/op. Histories
// also grow the number of untouched issues, comments and labels. FTS costs on
// the fixed-size touched issue and existing claim maintenance remain included.
// Run with -benchtime=1x: each iteration appends events to the same store, so
// repeated iterations grow the event history and make ns/op incomparable with
// the labeled starting history.
func BenchmarkFederationIngestWriterInterval(b *testing.B) {
	for _, history := range []int{100, 10_000, 53_000} {
		for _, batch := range []int{1, 10, 100} {
			b.Run(fmt.Sprintf("history=%d/batch=%d", history, batch), func(b *testing.B) {
				d, p, issue := incrementalTestStore(b)
				seedFederationBenchmarkHistory(b, d, p, history)
				reference := cloneIncrementalStore(b, d)
				ctx := b.Context()
				var writerStart time.Time
				var writerTotal, fullWriterTotal time.Duration
				d.federationIngestPrepared = func() { writerStart = time.Now() }
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					events := make([]db.FederationIngestEvent, batch)
					for j := range events {
						clock := int64(9_000_000_100_000 + i*batch + j)
						payload := fmt.Sprintf(`{"issue_uid":%q,"title":"batch-%d-%d"}`, issue.UID, i, j)
						events[j] = db.FederationIngestEvent{SourceEventID: int64(i*batch + j + 1), Event: incrementalRemoteEvent(b, p, issue.UID, "issue.updated", payload, clock)}
					}
					writerStart = time.Time{}
					b.StartTimer()
					result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: events[0].Event.OriginInstanceUID, Events: events})
					elapsed := time.Since(writerStart)
					b.StopTimer()
					require.NoError(b, err)
					require.Equal(b, batch, result.Accepted)
					require.False(b, writerStart.IsZero())
					writerTotal += elapsed
					fullStart := time.Now()
					_, err = ingestFullRebuildReference(ctx, reference, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: events[0].Event.OriginInstanceUID, Events: events})
					fullWriterTotal += time.Since(fullStart)
					require.NoError(b, err)
					b.StartTimer()
				}
				b.StopTimer()
				b.ReportMetric(float64(writerTotal.Nanoseconds())/float64(b.N), "writer-ns/op")
				b.ReportMetric(float64(fullWriterTotal.Nanoseconds())/float64(b.N), "full-writer-ns/op")
			})
		}
	}
}

func seedFederationBenchmarkHistory(b testing.TB, d *Store, p db.Project, count int) {
	b.Helper()
	ctx := b.Context()
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(b, err)
	defer func() { _ = tx.Rollback() }()
	var issueUID string
	validator := db.NewCronReplayValidator(tx, false)
	for i := range count {
		typ := "issue.updated"
		payload := ""
		if i%50 == 0 {
			issueUID, err = katauid.New()
			require.NoError(b, err)
			commentUID, err := katauid.New()
			require.NoError(b, err)
			// Seed the already-materialized baseline directly. Replaying thousands
			// of new issues here would benchmark fixture setup's short-ID allocation.
			shortID, err := shortid.Derive(issueUID, 8)
			require.NoError(b, err)
			res, err := tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,author,status,created_at,updated_at) VALUES(?,?,?,'history work','tester','open',?,?)`, issueUID, p.ID, shortID, "2026-05-23T12:00:00.000Z", "2026-05-23T12:00:00.000Z")
			require.NoError(b, err)
			id, err := res.LastInsertId()
			require.NoError(b, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at) VALUES(?,?,'tester','history comment',?)`, commentUID, id, "2026-05-23T12:00:00.000Z")
			require.NoError(b, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author,created_at) VALUES(?,'history','tester',?)`, id, "2026-05-23T12:00:00.000Z")
			require.NoError(b, err)
			typ = "issue.snapshot"
			payload = fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"history work","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z","labels":["history"],"comments":[{"comment_uid":%q,"author":"tester","body":"history comment","created_at":"2026-05-23T12:00:00.000Z"}]}`, issueUID, shortID, commentUID)
		} else {
			payload = fmt.Sprintf(`{"issue_uid":%q,"title":"history work"}`, issueUID)
		}
		ev := incrementalRemoteEvent(b, p, issueUID, typ, payload, 9_000_000_000_000+int64(i))
		inserted, err := insertFederationEventTx(ctx, tx, p.ID, p.Name, ev, validator)
		require.NoError(b, err)
		require.True(b, inserted)
	}
	require.NoError(b, tx.Commit())
}

// BenchmarkFederationIngestHistory measures the complete store ingest path;
// setup is excluded. Use -benchtime=1x so each case starts at the named size.
func BenchmarkFederationIngestHistory(b *testing.B) {
	for _, history := range []int{100, 10_000, 200_000} {
		for _, batch := range []int{1, 13} {
			b.Run(fmt.Sprintf("history=%d/batch=%d", history, batch), func(b *testing.B) {
				d, p, issue := incrementalTestStore(b)
				seedFederationBenchmarkHistory(b, d, p, history)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					events := make([]db.FederationIngestEvent, batch)
					for j := range events {
						events[j] = db.FederationIngestEvent{SourceEventID: int64(i*batch + j + 1), Event: incrementalRemoteEvent(b, p, issue.UID, "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":"batch-%d-%d"}`, issue.UID, i, j), int64(9_000_001_000_000+i*batch+j))}
					}
					b.StartTimer()
					pprof.Do(b.Context(), pprof.Labels("phase", "ingest"), func(ctx context.Context) {
						result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: events[0].Event.OriginInstanceUID, Events: events})
						require.NoError(b, err)
						require.Equal(b, batch, result.Accepted)
					})
				}
				b.StopTimer()
			})
		}
	}
}
