package sqlitestore

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Local writes and federated materialization share SQLite's single writer.
// Neither may shut the other out: materialization that restarts as soon as it
// commits must leave each local write a turn, and steady local writes must not
// make materialization fail.
func TestFederatedMaterializationSharesWriterWithLocalWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	spoke, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	for i := range 1000 {
		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.ID, Title: fmt.Sprint("issue ", i), Author: "tester"})
		require.NoError(t, err)
	}
	_, err = d.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: spoke.ID,
		HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EZ", Actor: "tester"})
	require.NoError(t, err)
	local, err := d.CreateProject(ctx, "local-project")
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, spoke.ID))

	// The local writer issues each write as a materialization finishes, when
	// the next one is already taking the writer: the worst moment to arrive.
	stop := make(chan struct{})
	finished := make(chan struct{}, 1)
	result := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count := 0
		for {
			select {
			case <-stop:
				result <- struct {
					count int
					err   error
				}{count, nil}
				return
			default:
			}
			if err := d.MaterializeFederatedProject(ctx, spoke.ID); err != nil {
				result <- struct {
					count int
					err   error
				}{count, err}
				return
			}
			count++
			select {
			case finished <- struct{}{}:
			default:
			}
		}
	}()

	const localWrites = 10
	var writeErr error
	for i := 0; i < localWrites && writeErr == nil; i++ {
		<-finished
		_, writeErr = d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: local.ID, Actor: "tester",
			Patch: map[string]jsontext.Value{"write": []byte(strconv.Itoa(i))}})
	}
	close(stop)
	materialized := <-result
	require.NoError(t, writeErr, "a local write must get a turn between materializations")
	require.NoError(t, materialized.err)
	require.LessOrEqual(t, materialized.count, 4*localWrites,
		"each local write should wait for at most about one materialization")
}

// Writers are admitted in arrival order, each as soon as the writer ahead
// releases the slot.
func TestWriteQueueAdmitsInArrivalOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		q := newWriteQueue()
		require.NoError(t, q.acquire(ctx))
		admitted := make(chan int, 3)
		for i := range 3 {
			go func() {
				if err := q.acquire(ctx); err == nil {
					admitted <- i
				}
			}()
			synctest.Wait()
		}
		for want := range 3 {
			q.release()
			require.Equal(t, want, <-admitted)
		}
	})
}

// A retrying writer must retain its place even when a write takes longer
// than SQLite's busy timeout. Virtual time makes the old five-second reset
// deterministic without slowing the suite down.
func TestWriteQueueRetryPreservesArrivalOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		q := newWriteQueue()
		require.NoError(t, q.acquire(ctx))
		admitted := make(chan int, 7)
		results := make(chan error, 7)
		startWriter := func(id int) {
			go func() {
				err := db.RetryTransient(ctx, IsTransient, func() error { return q.acquire(ctx) })
				if err == nil {
					admitted <- id
				}
				results <- err
			}()
			synctest.Wait()
		}
		startWriter(0)
		for id := 1; id <= 6; id++ {
			time.Sleep(time.Second)
			startWriter(id)
		}
		q.release()
		require.Equal(t, 0, <-admitted, "oldest writer must not move to the back on retry")
		for range 6 {
			q.release()
			<-admitted
		}
		q.release()
		for range 7 {
			require.NoError(t, <-results)
		}
	})
}

func TestWriteQueueCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newWriteQueue()
		require.NoError(t, q.acquire(t.Context()))
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- q.acquire(ctx) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		q.release()
		require.NoError(t, q.acquire(t.Context()))
		q.release()
	})
}

func TestWriteQueueAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	q := newWriteQueue()
	require.ErrorIs(t, q.acquire(ctx), context.Canceled)
	require.NoError(t, q.acquire(t.Context()))
	q.release()
}

func TestWriteQueueDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newWriteQueue()
		require.NoError(t, q.acquire(t.Context()))
		ctx, cancel := context.WithTimeout(t.Context(), 2*sqliteBusyTimeout)
		defer cancel()
		started := time.Now()
		require.ErrorIs(t, q.acquire(ctx), context.DeadlineExceeded)
		require.Equal(t, 2*sqliteBusyTimeout, time.Since(started))
		q.release()
		require.NoError(t, q.acquire(t.Context()))
		q.release()
	})
}

func TestWriteQueueCancellationRacesHandoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			q := newWriteQueue()
			require.NoError(t, q.acquire(t.Context()))
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				err := q.acquire(ctx)
				if err == nil {
					q.release()
				}
				result <- err
			}()
			synctest.Wait()
			// Both signals are ready before the waiter can resume.
			cancel()
			q.release()
			<-result
			require.NoError(t, q.acquire(t.Context()), "canceled waiter must pass the slot on")
			q.release()
		}
	})
}

// A settled schedule must stay readable while another operation owns the writer.
func TestReconcileDueNotificationSettledDoesNotWaitForWriter(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "settled-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delivered work", Author: "worker-a",
		Metadata: map[string]jsontext.Value{"scheduled_on": []byte(`"2020-01-01"`)},
	})
	require.NoError(t, err)
	in := db.ReconcileDueNotificationIn{IssueID: issue.ID, Now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC), DefaultTimezone: "UTC"}
	out, err := store.ReconcileDueNotification(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Changed)
	writer, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.Rollback()) }()
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err = store.ReconcileDueNotification(readCtx, in)
	require.NoError(t, err)
	require.False(t, out.Changed)
}

func TestReconcileDueNotificationInvalidRecipientDoesNotWaitForWriter(t *testing.T) {
	for _, recipient := range []string{"author", "owner"} {
		t.Run(recipient, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, err := store.CreateProject(ctx, "unaddressable-project")
			require.NoError(t, err)
			params := db.CreateIssueParams{
				ProjectID: project.ID, Title: "Unaddressable scheduled work", Author: "bad\nactor",
				Metadata: map[string]jsontext.Value{"scheduled_on": []byte(`"2020-01-01"`)},
			}
			if recipient == "owner" {
				params.Author = "worker-a"
				params.Owner = new("bad\nactor")
			}
			issue, _, err := store.CreateIssue(ctx, params)
			require.NoError(t, err)
			floor := time.Time{}
			in := db.ReconcileDueNotificationIn{
				IssueID: issue.ID, Now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
				DefaultTimezone: "UTC", ScheduledReachedFloor: &floor,
			}
			out, err := store.ReconcileDueNotification(ctx, in)
			require.NoError(t, err)
			require.Error(t, out.NotificationErr)
			require.NotNil(t, out.Reached)
			require.Nil(t, out.Event)

			writer, err := store.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, writer.Rollback()) }()
			readCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			out, err = store.ReconcileDueNotification(readCtx, in)
			require.NoError(t, err)
			require.Error(t, out.NotificationErr)
			require.False(t, out.Changed)
			require.Nil(t, out.Reached)
			require.Nil(t, out.Event)
		})
	}
}
