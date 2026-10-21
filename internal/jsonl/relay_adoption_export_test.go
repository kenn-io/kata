package jsonl_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/jsonl"
)

// Same-instance backups retain operation markers while project copies omit
// those markers and keep source content and ordinary metadata portable.
func TestFederationAdoptionOperationExport(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) db.Storage
	}{
		{"sqlite", func(t *testing.T) db.Storage { return openExportTestDB(t) }},
		{"postgres", func(t *testing.T) db.Storage {
			dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
			t.Cleanup(cleanup)
			s, err := pgstore.Open(t.Context(), dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			ctx := t.Context()
			store := backend.open(t)
			publication := dbtest.PublicationFixture(t, store.InstanceUID())
			published, err := store.(db.FederationPublicationStore).PublishFederationProject(ctx, publication, "publisher")
			require.NoError(t, err)
			require.False(t, published.Replayed)
			project, err := store.CreateProject(ctx, "adopting-project")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Portable source issue", Author: "member"})
			require.NoError(t, err)
			operations := store.(db.FederationAdoptionStore)
			op, err := operations.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", CatalogName: "hub", Account: "member", ProjectName: project.Name})
			require.NoError(t, err)
			native := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			adoptionKey := "federation.adoption.v1/" + project.UID
			publicationKey := "federation.publication.v1/" + publication.OperationUID
			var adoptionRaw, publicationRaw string
			require.NoError(t, native.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, adoptionKey).Scan(&adoptionRaw))
			require.NoError(t, native.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, publicationKey).Scan(&publicationRaw))
			var persisted db.FederationAdoptionOperation
			require.NoError(t, json.Unmarshal([]byte(adoptionRaw), &persisted))
			require.Equal(t, op, persisted)
			require.NotEmpty(t, publicationRaw)
			_, err = native.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, "portable-example", "portable-value")
			require.NoError(t, err)
			meta := map[string]string{}
			for record, err := range store.ExportMeta(ctx) {
				require.NoError(t, err)
				meta[record.Key] = record.Value
			}
			require.Equal(t, adoptionRaw, meta[adoptionKey], "native metadata reads include the durable pause")
			require.Equal(t, publicationRaw, meta[publicationKey], "native metadata reads include publication identity")
			require.Equal(t, "portable-value", meta["portable-example"])
			for _, scoped := range []bool{false, true} {
				opts := jsonl.ExportOptions{IncludeDeleted: true}
				if scoped {
					opts.ProjectID = project.ID
				}
				var out bytes.Buffer
				require.NoError(t, jsonl.Export(ctx, store, &out, opts))
				if scoped {
					require.NotContains(t, out.String(), "federation.adoption.v1/")
					require.NotContains(t, out.String(), "federation.publication.v1/")
				} else {
					require.Contains(t, out.String(), adoptionKey)
					require.Contains(t, out.String(), publicationKey)
				}
				records, err := jsonl.NewDecoder(bytes.NewReader(out.Bytes())).ReadAll(ctx)
				require.NoError(t, err)
				var ordinaryMeta, sourceIssue bool
				for _, record := range records {
					switch record.Kind {
					case jsonl.KindMeta:
						var m db.MetaKV
						require.NoError(t, json.Unmarshal(record.Data, &m))
						if m.Key == "portable-example" {
							require.Equal(t, "portable-value", m.Value)
							ordinaryMeta = true
						}
					case jsonl.KindIssue:
						var exported db.IssueExport
						require.NoError(t, json.Unmarshal(record.Data, &exported))
						if exported.UID == issue.UID {
							require.Equal(t, issue.Title, exported.Title)
							require.Equal(t, issue.Author, exported.Author)
							sourceIssue = true
						}
					}
				}
				require.True(t, ordinaryMeta, "ordinary metadata must survive export, scoped=%v", scoped)
				require.True(t, sourceIssue, "paused source content must remain readable, scoped=%v", scoped)
			}
			// Exclusion must not clear durable control state or unpause writes.
			retained, err := operations.FederationAdoption(ctx, project.ID)
			require.NoError(t, err)
			require.Equal(t, op, retained)
			var retainedPublication string
			require.NoError(t, native.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, publicationKey).Scan(&retainedPublication))
			require.Equal(t, publicationRaw, retainedPublication)
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Still paused", Author: "member"})
			require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
		})
	}
}
