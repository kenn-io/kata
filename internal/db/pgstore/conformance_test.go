package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
			payload json.RawMessage,
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
		SeedUnsupportedFederationEvent: func(ctx context.Context, store db.Storage, project db.Project, eventUID string) error {
			postgresStore := store.(*pgstore.Store)
			_, err := postgresStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES($1,$2,$3,$4,$5,$6,$7,1,0,$8)`, eventUID, store.InstanceUID(), project.ID,
				project.Name, "project.restored", "worker", `{}`, strings.Repeat("e", 64))
			return err
		},
		ExpectedFailures: pgstore.ExpectedConformanceFailures(),
	})
}
