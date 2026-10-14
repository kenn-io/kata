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
	ctx := context.Background()
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
		ctx := context.Background()
		q := newWriteQueue(time.Minute)
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

// A writer that waits past the busy timeout gives up with an error the store
// retries as busy contention, and the writers behind it keep their turns.
func TestWriteQueueTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		q := newWriteQueue(sqliteBusyTimeout)
		require.NoError(t, q.acquire(ctx))
		admitted := make(chan error, 1)
		start := time.Now()
		err := q.acquire(ctx)
		require.ErrorAs(t, err, &writeQueueBusyError{})
		require.True(t, IsTransient(err), "a write queue timeout must be retried like SQLite busy contention")
		require.Equal(t, sqliteBusyTimeout, time.Since(start))
		go func() { admitted <- q.acquire(ctx) }()
		synctest.Wait()
		q.release()
		require.NoError(t, <-admitted, "a writer that gave up must not keep the slot")
	})
}
