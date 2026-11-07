package dbtest

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type cronReadGate struct {
	query interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (q *cronReadGate) QueryContext(ctx context.Context, s string, args ...any) (*sql.Rows, error) {
	rows, err := q.query.QueryContext(ctx, s, args...)
	if err == nil {
		q.once.Do(func() {
			close(q.reached)
			select {
			case <-q.release:
			case <-ctx.Done():
			}
		})
	}
	return rows, err
}
func (q *cronReadGate) QueryRowContext(ctx context.Context, s string, args ...any) *sql.Row {
	return q.query.QueryRowContext(ctx, s, args...)
}

// Contract: definition and cutoff are one read snapshot even if resume commits
// while that read is in flight. Each pair is wholly before or wholly after.
func checkCronControlReads(t *testing.T, _ db.Storage, b Backend) error {
	for _, list := range []bool{false, true} {
		t.Run(map[bool]string{false: "show", true: "list"}[list], func(t *testing.T) {
			s := b.Open(t)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			p, _, job, _ := recurringFixture(t, s, "all")
			def := job.Definition
			def.Enabled = false
			job, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
			require.NoError(t, err)
			q := &cronReadGate{query: s.(interface {
				QueryContext(context.Context, string, ...any) (*sql.Rows, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			}), reached: make(chan struct{}), release: make(chan struct{})}
			reader := db.CronSQL{Query: q}
			type result struct {
				read db.CronJobControlRead
				err  error
			}
			done := make(chan result, 1)
			go func() {
				if list {
					reads, err := reader.JobControlReads(t.Context(), p.ID, false)
					var read db.CronJobControlRead
					if len(reads) > 0 {
						read = reads[0]
					}
					done <- result{read, err}
				} else {
					read, err := reader.JobControlRead(t.Context(), p.ID, job.UID)
					done <- result{read, err}
				}
			}()
			<-q.reached
			other := b.OpenSame(t, s)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			_, err = other.ResumeCronJob(t.Context(), db.ResumeCronJobIn{ProjectID: p.ID, UID: job.UID, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, DiscardMissed: true})
			close(q.release)
			require.NoError(t, err)
			got := <-done
			require.NoError(t, got.err)
			require.Equal(t, got.read.Job.Definition.Enabled, got.read.Passover != nil)
			read, err := s.CronJobControlRead(t.Context(), p.ID, job.UID)
			require.NoError(t, err)
			require.True(t, read.Job.Definition.Enabled)
			require.NotNil(t, read.Passover)
			reads, err := s.ListCronJobControlReads(t.Context(), p.ID, false)
			require.NoError(t, err)
			require.Len(t, reads, 1)
			require.Equal(t, read, reads[0])
		})
	}
	return nil
}
