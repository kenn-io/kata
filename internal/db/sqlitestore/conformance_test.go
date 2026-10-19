package sqlitestore_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestStorageConformance(t *testing.T) {
	t.Parallel()
	dbtest.RunStorageConformance(t, dbtest.Backend{
		Name: "sqlite",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			return store
		},
		OpenSame: func(t *testing.T, existing db.Storage) db.Storage {
			other, err := sqlitestore.Open(t.Context(), existing.(*sqlitestore.Store).Path())
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })
			return other
		},
		InstallExternalRootClock: func(store db.Storage, now func() time.Time) func() {
			return sqlitestore.InstallExternalRootClockForTest(store.(*sqlitestore.Store), now)
		},
		InstallRelayHistoryFullScanObserver: func(store db.Storage, observe func()) func() {
			return sqlitestore.InstallRelayHistoryFullScanObserverForTest(store.(*sqlitestore.Store), observe)
		},
		OpenWithRelayCheckpointCapacity: func(t *testing.T, capacity int64) db.Storage {
			t.Helper()
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"), db.WithRelayCheckpointCapacity(capacity))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
		BackdateCommentCreated: backdateCommentCreated,
		SeedLegacyPendingClaim: func(ctx context.Context, store db.Storage, requestUID string) error {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx,
				`UPDATE pending_claim_requests SET holder_instance_uid = '' WHERE request_uid = ?`, requestUID)
			return err
		},
		SeedClaimViolation: func(
			ctx context.Context,
			store db.Storage,
			project db.Project,
			issue db.Issue,
			eventUID string,
			payload jsontext.Value,
		) error {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES(?,?,?,?,?,?,'claim.violated','audit',?,1,0,?)`, eventUID,
				store.InstanceUID(), project.ID, project.Name, issue.ID, issue.UID,
				string(payload), strings.Repeat("c", 64))
			return err
		},
		DetachEventIssueID: func(ctx context.Context, store db.Storage, eventID int64) error {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx, `UPDATE events SET issue_id = NULL WHERE id = ?`, eventID)
			return err
		},
		SeedUnsupportedFederationEvent: func(ctx context.Context, store db.Storage, project db.Project, eventUID string) error {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES(?,?,?,?,?,?,?,1,0,?)`, eventUID, store.InstanceUID(), project.ID, project.Name,
				"project.restored", "worker", `{}`, strings.Repeat("e", 64))
			return err
		},
		InstallEnrollmentInsertFailure: func(ctx context.Context, store db.Storage) (func() error, error) {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx, `
				CREATE TRIGGER fail_federation_enrollment_rotation_insert
				BEFORE INSERT ON federation_enrollments
				BEGIN
					SELECT RAISE(FAIL, 'injected enrollment rotation insert failure');
				END`)
			return func() error {
				_, dropErr := sqlStore.ExecContext(
					context.Background(),
					`DROP TRIGGER IF EXISTS fail_federation_enrollment_rotation_insert`,
				)
				return dropErr
			}, err
		},
		InstallEnrollmentRotationStage: func(
			store db.Storage,
			stage func(context.Context) error,
		) func() {
			return sqlitestore.InstallEnrollmentRotationStageForTest(
				store.(*sqlitestore.Store),
				stage,
			)
		},
		SetArtifactStagingExpiry: func(ctx context.Context, store db.Storage, projectUID, digest, expiry string) error {
			result, err := store.(*sqlitestore.Store).ExecContext(ctx,
				`UPDATE federation_embedding_artifacts SET staging_expires_at=? WHERE project_uid=? AND digest=? AND staging_expires_at IS NOT NULL`,
				expiry, projectUID, digest)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return fmt.Errorf("set staging expiry on one staged artifact: rows=%d: %w", n, err)
			}
			return nil
		},
		ArtifactStagingExpiry: func(ctx context.Context, store db.Storage, projectUID, digest string) (*string, error) {
			var expiry sql.NullString
			err := store.(*sqlitestore.Store).QueryRowContext(ctx,
				`SELECT staging_expires_at FROM federation_embedding_artifacts WHERE project_uid=? AND digest=?`,
				projectUID, digest).Scan(&expiry)
			if err != nil || !expiry.Valid {
				return nil, err
			}
			return &expiry.String, nil
		},
		InstallRelayAckRowCounter: installRelayAckRowCounter,
	})
}

// installRelayAckRowCounter counts, with a real row trigger, every outbox row
// that an acknowledgement UPDATE matches, including rows whose value does not
// change.
func installRelayAckRowCounter(ctx context.Context, store db.Storage) (func(context.Context) (int64, error), func() error, error) {
	sqlStore := store.(*sqlitestore.Store)
	for _, stmt := range []string{
		`CREATE TABLE relay_ack_row_updates(n INTEGER NOT NULL)`,
		`INSERT INTO relay_ack_row_updates(n) VALUES(0)`,
		`CREATE TRIGGER count_relay_ack_row_updates AFTER UPDATE OF acknowledged ON federation_relay_outbox
		BEGIN
			UPDATE relay_ack_row_updates SET n=n+1;
		END`,
	} {
		if _, err := sqlStore.ExecContext(ctx, stmt); err != nil {
			return nil, nil, err
		}
	}
	count := func(ctx context.Context) (int64, error) {
		var n int64
		err := sqlStore.QueryRowContext(ctx, `SELECT n FROM relay_ack_row_updates`).Scan(&n)
		return n, err
	}
	remove := func() error {
		for _, stmt := range []string{
			`DROP TRIGGER IF EXISTS count_relay_ack_row_updates`,
			`DROP TABLE IF EXISTS relay_ack_row_updates`,
		} {
			if _, err := sqlStore.ExecContext(context.Background(), stmt); err != nil {
				return err
			}
		}
		return nil
	}
	return count, remove, nil
}

func TestExternalRootConformance(t *testing.T) {
	t.Parallel()
	dbtest.RunExternalRootConformance(t, dbtest.Backend{
		Name: "sqlite",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			return store
		},
		InstallExternalRootClock: func(store db.Storage, now func() time.Time) func() {
			return sqlitestore.InstallExternalRootClockForTest(store.(*sqlitestore.Store), now)
		},
		BackdateCommentCreated: backdateCommentCreated,
	})
}

func TestExternalRootContentOwned(t *testing.T) {
	t.Parallel()
	dbtest.RunExternalRootContentOwnershipConformance(t, dbtest.Backend{
		Name: "sqlite",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			return store
		},
	})
}

func backdateCommentCreated(ctx context.Context, store db.Storage, commentID int64, createdAt time.Time) error {
	sqlStore := store.(*sqlitestore.Store)
	_, err := sqlStore.ExecContext(ctx,
		`UPDATE comments SET created_at=? WHERE id=?`,
		createdAt.UTC().Format(time.RFC3339Nano), commentID)
	return err
}
