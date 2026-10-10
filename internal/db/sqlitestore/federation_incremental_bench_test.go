package sqlitestore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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
				ctx := context.Background()
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

func seedFederationBenchmarkHistory(b *testing.B, d *Store, p db.Project, count int) {
	b.Helper()
	ctx := context.Background()
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
			typ = "issue.snapshot"
			payload = fmt.Sprintf(`{"uid":%q,"title":"history work","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z","labels":["history"],"comments":[{"comment_uid":%q,"author":"tester","body":"history comment","created_at":"2026-05-23T12:00:00.000Z"}]}`, issueUID, commentUID)
		} else {
			payload = fmt.Sprintf(`{"issue_uid":%q,"title":"history work"}`, issueUID)
		}
		ev := incrementalRemoteEvent(b, p, issueUID, typ, payload, 9_000_000_000_000+int64(i))
		inserted, err := insertFederationEventTx(ctx, tx, p.ID, p.Name, ev, validator)
		require.NoError(b, err)
		require.True(b, inserted)
	}
	require.NoError(b, tx.Commit())
	require.NoError(b, d.MaterializeFederatedProject(ctx, p.ID))
}
