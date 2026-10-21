package pgstore_test

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestFederationAdoptionPersistence(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	dbtest.RunFederationAdoptionPersistence(t, s, func() db.Storage {
		again, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		return again
	})
}

func TestFederationAdoptionCoordination(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	other, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project, err := s.CreateProject(ctx, "coordination-project")
	require.NoError(t, err)
	op := db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: s.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", Account: "member", CatalogName: "hub", ProjectName: project.Name}
	// Block the actual writer after its native gate, at the event ordering lock.
	blocker, err := s.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('events_sequence'))`)
	require.NoError(t, err)
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	pids := make(chan int, 8)
	writer := make(chan error, 1)
	go func() {
		_, _, err := other.CreateIssue(db.WithTransactionFence(ctx, func(c context.Context, tx db.Transaction) error {
			var pid int
			err := tx.QueryRowContext(c, `SELECT pg_backend_pid()`).Scan(&pid)
			pids <- pid
			return err
		}), db.CreateIssueParams{ProjectID: project.ID, Title: "admitted", Author: "member"})
		writer <- err
	}()
	pid := <-pids
	require.Eventually(t, func() bool { //nolint:kennlint // PostgreSQL lock state is observable only through pg_blocking_pids.
		var blocked bool
		err := s.QueryRowContext(ctx, `SELECT $1=ANY(pg_blocking_pids($2))`, blockerPID, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	frozen := make(chan error, 1)
	go func() {
		_, err := s.BeginFederationAdoption(ctx, op)
		frozen <- err
	}()
	early := false
	select {
	case err := <-frozen:
		early = true
		assert.Fail(t, "freeze passed an admitted writer", "error: %v", err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // Absence window: the held native lock must keep the competing operation blocked.
	}
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-writer)
	if !early {
		require.NoError(t, <-frozen)
	}
	op, err = s.FederationAdoption(ctx, project.ID)
	require.NoError(t, err)
	_, _, err = other.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "late", Author: "member"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	// Every descriptor transition, including CAS, conflicts with project readers.
	held, err := other.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = held.Rollback() }()
	var id int64
	require.NoError(t, held.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR SHARE`, project.ID).Scan(&id))
	next := op
	next.Phase = "frozen"
	next.Digest = "digest"
	next.EnvelopePath = "path"
	advanced := make(chan error, 1)
	go func() { advanced <- s.AdvanceFederationAdoption(ctx, op, next) }()
	early = false
	select {
	case err := <-advanced:
		early = true
		assert.Fail(t, "CAS did not take exclusive operation lock", "error: %v", err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // Absence window: the held native lock must keep the competing operation blocked.
	}
	require.NoError(t, held.Commit())
	if !early {
		require.NoError(t, <-advanced)
	}
	// Both connections compete against one complete expected descriptor.
	a := next
	a.Phase = "transmitting"
	b := next
	b.Phase = "rejected"
	results := make(chan error, 2)
	go func() { results <- s.AdvanceFederationAdoption(ctx, next, a) }()
	go func() { results <- other.AdvanceFederationAdoption(ctx, next, b) }()
	e1, e2 := <-results, <-results
	require.NotEqual(t, e1 == nil, e2 == nil, "only one CAS may commit")
}

func TestFederationAdoptionStaleSnapshot(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	other, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	project, err := s.CreateProject(ctx, "snapshot-project")
	require.NoError(t, err)
	snapshot := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	first := true
	fence := func(c context.Context, tx db.Transaction) error {
		var count int
		err := tx.QueryRowContext(c, `SELECT COUNT(*) FROM projects`).Scan(&count)
		if first {
			first = false
			close(snapshot)
			select {
			case <-resume:
			case <-c.Done():
				return c.Err()
			}
		}
		return err
	}
	go func() {
		_, _, err := other.CreateIssue(db.WithTransactionFence(ctx, fence), db.CreateIssueParams{ProjectID: project.ID, Title: "stale writer", Author: "member"})
		result <- err
	}()
	<-snapshot
	_, err = s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: s.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", Account: "member", CatalogName: "hub", ProjectName: project.Name})
	close(resume)
	require.NoError(t, err)
	require.ErrorIs(t, <-result, db.ErrFederationAdoptionPaused, "a pre-freeze snapshot cannot admit a post-freeze mutation")
}

// A pin admitted before Begin must commit before eligibility is evaluated.
func TestFederationAdoptionRootPinDrainsBeforeEligibility(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	other, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project, err := s.CreateProject(ctx, "root-pin-project")
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	blocker, err := s.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `LOCK TABLE federation_root_keys IN SHARE MODE`)
	require.NoError(t, err)
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	pids := make(chan int, 8)
	writer := make(chan error, 1)
	go func() {
		writer <- other.PinRootAuthority(db.WithTransactionFence(ctx, func(c context.Context, tx db.Transaction) error {
			var pid int
			err := tx.QueryRowContext(c, `SELECT pg_backend_pid()`).Scan(&pid)
			pids <- pid
			return err
		}), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000012", KeyID: db.RootPublicKeyID(public), PublicKey: public})
	}()
	pid := <-pids
	require.Eventually(t, func() bool { //nolint:kennlint // PostgreSQL lock state is observable only through pg_blocking_pids.
		var blocked bool
		err := s.QueryRowContext(ctx, `SELECT $1=ANY(pg_blocking_pids($2))`, blockerPID, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	frozen := make(chan error, 1)
	go func() {
		_, err := s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: s.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", Account: "member", CatalogName: "hub", ProjectName: project.Name})
		frozen <- err
	}()
	select {
	case err := <-frozen:
		t.Fatalf("Begin passed the admitted root-pin writer: %v", err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // Absence window while the native pin writer is blocked after admission.
	}
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-writer)
	require.ErrorIs(t, <-frozen, db.ErrFederationAdoptionConflict)
	_, err = s.FederationAdoption(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
