package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestStorageConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	var schemaNumber int
	dbtest.RunStorageConformance(t, dbtest.Backend{
		Name: "postgres",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			schemaNumber++
			schema := fmt.Sprintf("conformance_%d", schemaNumber)
			t.Cleanup(func() {
				_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			})

			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema:     schema,
				SchemaMode: pgstore.SchemaModeBootstrap,
			})
			require.NoError(t, err)
			return store
		},
		OpenSame: func(t *testing.T, existing db.Storage) db.Storage {
			var schema string
			require.NoError(t, existing.(*pgstore.Store).QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
			other, err := pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })
			return other
		},
		InstallExternalRootClock: func(store db.Storage, now func() time.Time) func() {
			return pgstore.InstallExternalRootClockForTest(store.(*pgstore.Store), now)
		},
		InstallRelayHistoryFullScanObserver: func(store db.Storage, observe func()) func() {
			return pgstore.InstallRelayHistoryFullScanObserverForTest(store.(*pgstore.Store), observe)
		},
		OpenWithRelayCheckpointCapacity: func(t *testing.T, capacity int64) db.Storage {
			t.Helper()
			schemaNumber++
			schema := fmt.Sprintf("conformance_%d", schemaNumber)
			t.Cleanup(func() {
				_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			})
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap}, db.WithRelayCheckpointCapacity(capacity))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
		BackdateCommentCreated: backdateCommentCreated,
		SeedLegacyPendingClaim: func(ctx context.Context, store db.Storage, requestUID string) error {
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx,
				`UPDATE pending_claim_requests SET holder_instance_uid = '' WHERE request_uid = $1`, requestUID)
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
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES($1,$2,$3,$4,$5,$6,'claim.violated','audit',$7,1,0,$8)`, eventUID,
				store.InstanceUID(), project.ID, project.Name, issue.ID, issue.UID,
				string(payload), strings.Repeat("c", 64))
			return err
		},
		DetachEventIssueID: func(ctx context.Context, store db.Storage, eventID int64) error {
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx, `UPDATE events SET issue_id = NULL WHERE id = $1`, eventID)
			return err
		},
		SeedUnsupportedFederationEvent: func(ctx context.Context, store db.Storage, project db.Project, eventUID string) error {
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES($1,$2,$3,$4,$5,$6,$7,1,0,$8)`, eventUID, store.InstanceUID(), project.ID,
				project.Name, "project.restored", "worker", `{}`, strings.Repeat("e", 64))
			return err
		},
		InstallEnrollmentInsertFailure: func(ctx context.Context, store db.Storage) (func() error, error) {
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx, `
				CREATE FUNCTION fail_federation_enrollment_rotation_insert()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $$
				BEGIN
					RAISE EXCEPTION 'injected enrollment rotation insert failure';
				END
				$$;
				CREATE TRIGGER fail_federation_enrollment_rotation_insert
				BEFORE INSERT ON federation_enrollments
				FOR EACH ROW
				EXECUTE FUNCTION fail_federation_enrollment_rotation_insert()`)
			return func() error {
				if _, dropErr := postgresStore.ExecContext(
					context.Background(),
					`DROP TRIGGER IF EXISTS fail_federation_enrollment_rotation_insert ON federation_enrollments`,
				); dropErr != nil {
					return dropErr
				}
				_, dropErr := postgresStore.ExecContext(
					context.Background(),
					`DROP FUNCTION IF EXISTS fail_federation_enrollment_rotation_insert()`,
				)
				return dropErr
			}, err
		},
		InstallEnrollmentRotationStage: func(
			store db.Storage,
			stage func(context.Context) error,
		) func() {
			return pgstore.InstallEnrollmentRotationStageForTest(
				store.(*pgstore.Store),
				stage,
			)
		},
		SetArtifactStagingExpiry: func(ctx context.Context, store db.Storage, projectUID, digest, expiry string) error {
			result, err := store.(*pgstore.Store).ExecContext(ctx,
				`UPDATE federation_embedding_artifacts SET staging_expires_at=$1 WHERE project_uid=$2 AND digest=$3 AND staging_expires_at IS NOT NULL`,
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
			err := store.(*pgstore.Store).QueryRowContext(ctx,
				`SELECT staging_expires_at FROM federation_embedding_artifacts WHERE project_uid=$1 AND digest=$2`,
				projectUID, digest).Scan(&expiry)
			if err != nil || !expiry.Valid {
				return nil, err
			}
			return &expiry.String, nil
		},
		InstallRelayAckRowCounter: installRelayAckRowCounter,
		ExpectedFailures:          pgstore.ExpectedConformanceFailures(),
	})
}

// installRelayAckRowCounter counts, with a real row trigger, every outbox row
// that an acknowledgement UPDATE matches, including rows whose value does not
// change.
func installRelayAckRowCounter(ctx context.Context, store db.Storage) (func(context.Context) (int64, error), func() error, error) {
	postgresStore := store.(*pgstore.Store)
	for _, stmt := range []string{
		`CREATE TABLE relay_ack_row_updates(n BIGINT NOT NULL)`,
		`INSERT INTO relay_ack_row_updates(n) VALUES(0)`,
		`CREATE FUNCTION count_relay_ack_row_updates()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			UPDATE relay_ack_row_updates SET n=n+1;
			RETURN NULL;
		END
		$$`,
		`CREATE TRIGGER count_relay_ack_row_updates
		AFTER UPDATE OF acknowledged ON federation_relay_outbox
		FOR EACH ROW
		EXECUTE FUNCTION count_relay_ack_row_updates()`,
	} {
		if _, err := postgresStore.ExecContext(ctx, stmt); err != nil {
			return nil, nil, err
		}
	}
	count := func(ctx context.Context) (int64, error) {
		var n int64
		err := postgresStore.QueryRowContext(ctx, `SELECT n FROM relay_ack_row_updates`).Scan(&n)
		return n, err
	}
	remove := func() error {
		for _, stmt := range []string{
			`DROP TRIGGER IF EXISTS count_relay_ack_row_updates ON federation_relay_outbox`,
			`DROP FUNCTION IF EXISTS count_relay_ack_row_updates()`,
			`DROP TABLE IF EXISTS relay_ack_row_updates`,
		} {
			if _, err := postgresStore.ExecContext(context.Background(), stmt); err != nil {
				return err
			}
		}
		return nil
	}
	return count, remove, nil
}

func TestExternalRootConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	var schemaNumber int
	dbtest.RunExternalRootConformance(t, dbtest.Backend{
		Name: "postgres",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			schemaNumber++
			schema := fmt.Sprintf("external_root_conformance_%d", schemaNumber)
			t.Cleanup(func() {
				_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			})
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema:     schema,
				SchemaMode: pgstore.SchemaModeBootstrap,
			})
			require.NoError(t, err)
			return store
		},
		InstallExternalRootClock: func(store db.Storage, now func() time.Time) func() {
			return pgstore.InstallExternalRootClockForTest(store.(*pgstore.Store), now)
		},
		BackdateCommentCreated: backdateCommentCreated,
	})
}

func TestExternalRootContentOwned(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var schemaNumber int
	dbtest.RunExternalRootContentOwnershipConformance(t, dbtest.Backend{
		Name: "postgres",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			schemaNumber++
			schema := fmt.Sprintf("external_root_content_owned_%d", schemaNumber)
			t.Cleanup(func() {
				_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			})
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
			})
			require.NoError(t, err)
			return store
		},
	})
}

func backdateCommentCreated(ctx context.Context, store db.Storage, commentID int64, createdAt time.Time) error {
	postgresStore := store.(*pgstore.Store)
	_, err := postgresStore.ExecContext(ctx,
		`UPDATE comments SET created_at=$1 WHERE id=$2`,
		createdAt.UTC(), commentID)
	return err
}
