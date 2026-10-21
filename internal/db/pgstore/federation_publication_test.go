package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

// The native event lock may queue writes, but publication never takes a
// restore serving lease or blocks unrelated reads/connections.
func TestPublicationConcurrentIDsAndServing(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	unrelated, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	p := dbtest.PublicationFixture(t, store.InstanceUID())
	// A test-local trigger pauses the first historical insertion while all
	// project/projection rows and the native event sequence lock are retained.
	_, err = store.ExecContext(ctx, `CREATE FUNCTION publication_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.origin_instance_uid='01K00000000000000000000004' THEN PERFORM pg_advisory_xact_lock(74321,1); END IF; RETURN NEW; END $$`)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `CREATE TRIGGER publication_pause BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION publication_pause()`)
	require.NoError(t, err)
	holder, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(74321,1)`)
	require.NoError(t, err)
	var holderPID int
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	type outcome struct {
		result db.FederationPublicationResult
		err    error
	}
	published := make(chan outcome, 1)
	go func() { r, e := store.PublishFederationProject(ctx, p, "publisher"); published <- outcome{r, e} }()
	//nolint:kennlint // Native PostgreSQL lock observation cannot run inside synctest.
	require.Eventually(t, func() bool {
		var blocked bool
		require.NoError(t, store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked))
		return blocked
	}, 5*time.Second, 10*time.Millisecond)
	// Health uses this same live pool; no real daemon or credentials are used.
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, store.PingContext(readCtx))
	got, err := store.ProjectByID(readCtx, unrelated.ID)
	require.NoError(t, err)
	require.Equal(t, unrelated.UID, got.UID)
	// Competing native identity allocation may finish its event only after the
	// publication releases events_sequence. It must not reuse an imported ID.
	created := make(chan error, 1)
	go func() {
		_, _, e := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: unrelated.ID, Title: "Concurrent native insert", Author: "writer"})
		created <- e
	}()
	require.NoError(t, holder.Commit())
	var result db.FederationPublicationResult
	select {
	case done := <-published:
		require.NoError(t, done.err)
		result = done.result
	case <-time.After(10 * time.Second):
		t.Fatal("publication did not resume")
	}
	select {
	case err := <-created:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("native writer did not resume")
	}
	var count int
	require.NoError(t, store.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues`).Scan(&count))
	require.Equal(t, 3, count)
	events, err := store.EventsByUIDs(ctx, result.Project.ID, []string{"01K00000000000000000000008", "01K00000000000000000000009"})
	require.NoError(t, err)
	require.Len(t, events, 2)
	byUID := map[string]db.Event{}
	for _, e := range events {
		byUID[e.UID] = e
	}
	require.Less(t, byUID["01K00000000000000000000008"].ID, byUID["01K00000000000000000000009"].ID)
	require.Greater(t, result.CatalogEvent.ID, byUID["01K00000000000000000000009"].ID)
}

func TestPublicationCommitFences(t *testing.T) {
	for _, kind := range []string{"revoked_token", "changed_host_policy"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := openConformanceStore(t)
			t.Cleanup(func() { _ = store.Close() })
			ctx := t.Context()
			p := dbtest.PublicationFixture(t, store.InstanceUID())
			// Model admission, then change the admitted authority before the native
			// transaction begins. Its fence must reject before any domain insertion.
			denied := errors.New("host policy changed after admission")
			var fenced context.Context
			if kind == "revoked_token" {
				token, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "synthetic-publication-account", Actor: "publisher", AdminActor: "owner"})
				require.NoError(t, err)
				fenced = db.WithTransactionFence(ctx, db.ActorTokenTransactionFence(token))
				_, err = store.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=CURRENT_TIMESTAMP WHERE id=$1`, token.ID)
				require.NoError(t, err)
			} else {
				var admittedRevision string
				require.NoError(t, store.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='project_access_revision'`).Scan(&admittedRevision))
				fenced = db.WithTransactionFence(ctx, func(fenceCtx context.Context, tx db.Transaction) error {
					var current string
					if err := tx.QueryRowContext(fenceCtx, `SELECT value FROM meta WHERE key='project_access_revision' FOR SHARE`).Scan(&current); err != nil {
						return err
					}
					if current != admittedRevision {
						return denied
					}
					return nil
				})
				_, _, err := store.CreateTeam(ctx, "changed-policy-team", "owner")
				require.NoError(t, err)
			}
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			_, err = store.PublishFederationProject(fenced, p, "publisher")
			if kind == "revoked_token" {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.ErrorIs(t, err, denied)
			}
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, after)
			for _, query := range []string{`SELECT COUNT(*) FROM projects WHERE uid=$1`, `SELECT COUNT(*) FROM meta WHERE key='federation.publication.v1/' || $1`} {
				var count int
				if query == `SELECT COUNT(*) FROM projects WHERE uid=$1` {
					require.NoError(t, store.QueryRowContext(ctx, query, p.ProjectUID).Scan(&count))
				} else {
					require.NoError(t, store.QueryRowContext(ctx, query, p.OperationUID).Scan(&count))
				}
				require.Zero(t, count)
			}
		})
	}
}

var _ db.FederationPublicationStore = (*pgstore.Store)(nil)
