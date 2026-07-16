package sqlitestore_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestStorageConformance(t *testing.T) {
	dbtest.RunStorageConformance(t, dbtest.Backend{
		Name: "sqlite",
		Open: func(t *testing.T) db.Storage {
			t.Helper()
			store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			return store
		},
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
			payload json.RawMessage,
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
		SeedUnsupportedFederationEvent: func(ctx context.Context, store db.Storage, project db.Project, eventUID string) error {
			sqlStore := store.(*sqlitestore.Store)
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,type,actor,payload,
hlc_physical_ms,hlc_counter,content_hash
) VALUES(?,?,?,?,?,?,?,1,0,?)`, eventUID, store.InstanceUID(), project.ID, project.Name,
				"project.restored", "worker", `{}`, strings.Repeat("e", 64))
			return err
		},
	})
}
