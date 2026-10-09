package jsonl_test

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestStandaloneCopyPreservesContentWithoutRestoringAuthority(t *testing.T) {
	ctx := t.Context()
	source := openExportTestDB(t)
	first, err := source.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := source.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	archive, err := source.CreateProject(ctx, "archived-project")
	require.NoError(t, err)
	for _, p := range []db.Project{first, second} {
		_, err = source.EnableProjectFederation(ctx, p.ID, "operator")
		require.NoError(t, err)
	}
	one := createTesterIssue(ctx, t, source, first.ID, "First task", "Historical body", "label-a")
	two := createTesterIssue(ctx, t, source, second.ID, "Second task", "Another body", "label-b")
	deleted := createTesterIssue(ctx, t, source, first.ID, "Deleted task", "Retained content")
	_, _, _, err = source.SoftDeleteIssue(ctx, deleted.ID, "user-a")
	require.NoError(t, err)
	createTesterIssue(ctx, t, source, archive.ID, "Archived task", "Still archived")
	_, _, err = source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: archive.ID, Actor: "user-a", Force: true})
	require.NoError(t, err)
	addTesterComment(ctx, t, source, one.ID, "Historical comment")
	_, _, err = source.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: one.ID, ToIssueID: two.ID, Type: "related", Author: "user-b",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: one.ID,
		FromShortID: one.ShortID, FromUID: one.UID, ToShortID: two.ShortID, ToUID: two.UID, Actor: "user-b",
	})
	require.NoError(t, err)
	principal := db.ClaimPrincipal{HolderInstanceUID: source.InstanceUID(), Holder: "user-a", ClientKind: "cli"}
	_, err = source.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: first.ID, IssueRef: one.UID, Principal: principal, ClaimKind: "hard", Now: time.Now(),
	})
	require.NoError(t, err)
	_, err = source.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: second.ID, IssueRef: two.UID, Principal: principal, ClaimKind: "hard", Now: time.Now(),
	})
	require.NoError(t, err)
	const token = "synthetic-original-api-token"
	_, _, err = source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: token, Actor: "user-a", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	revoked, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "synthetic-revoked-api-token", Actor: "user-b", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = source.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
	require.NoError(t, err)
	require.NoError(t, source.RecordFederationSyncPullStarted(ctx, first.ID, time.Now()))
	_, err = source.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: first.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 1, LastEventID: 2, EventUIDs: []string{"source-event"},
		Error: "synthetic pending batch", CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	input := exportToBuffer(ctx, t, source).Bytes()

	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var copied db.Storage
			if backend == "sqlite" {
				copied = openImportTargetDB(t)
			} else {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				pg, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, pg.Close()) })
				copied = pg
			}
			identity := copied.InstanceUID()
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(input), copied, jsonl.ImportOptions{AsStandalone: true}))
			assert.Equal(t, identity, copied.InstanceUID())
			assert.NotEqual(t, source.InstanceUID(), copied.InstanceUID())
			bindings, err := copied.ListFederationBindings(ctx)
			require.NoError(t, err)
			assert.Empty(t, bindings)
			count, err := copied.CountLiveClaims(ctx, first.ID)
			require.NoError(t, err)
			assert.Zero(t, count)
			count, err = copied.CountPendingClaims(ctx, second.ID)
			require.NoError(t, err)
			assert.Zero(t, count)
			_, err = copied.ResolveAPIToken(ctx, token)
			assert.ErrorIs(t, err, db.ErrNotFound)

			var snapshot bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, copied, &snapshot, jsonl.ExportOptions{IncludeDeleted: true}))
			records, err := jsonl.NewDecoder(bytes.NewReader(snapshot.Bytes())).ReadAll(ctx)
			require.NoError(t, err)
			for _, record := range records {
				assert.NotContains(t, []jsonl.Kind{jsonl.KindFederationBinding, jsonl.KindFederationSyncStatus,
					jsonl.KindFederationQuarantine, jsonl.KindFederationEnrollment,
					jsonl.KindIssueClaim, jsonl.KindPendingClaimRequest}, record.Kind)
			}
			assert.Equal(t, standaloneContent(t, input), standaloneContent(t, snapshot.Bytes()),
				"content, cross-project links, archived/deleted rows and historical events retain their identities")
			// Removing only the token projection would let a later ordinary
			// restore silently recreate the source's token authority.
			restored := openImportTargetDB(t)
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(snapshot.Bytes()), restored))
			_, err = restored.ResolveAPIToken(ctx, token)
			assert.ErrorIs(t, err, db.ErrNotFound)
			assert.Equal(t, standaloneContent(t, input), standaloneContent(t, exportToBuffer(ctx, t, restored).Bytes()))
			// Refusing a second replay also proves the copy cannot overwrite
			// an already initialized destination through the library path.
			err = jsonl.ImportWithOptions(ctx, bytes.NewReader(input), copied, jsonl.ImportOptions{AsStandalone: true})
			require.Error(t, err)
		})
	}
	assert.Equal(t, input, exportToBuffer(ctx, t, source).Bytes(), "copying never mutates the source")
}

func standaloneContent(t *testing.T, data []byte) []string {
	t.Helper()
	records, err := jsonl.NewDecoder(bytes.NewReader(data)).ReadAll(t.Context())
	require.NoError(t, err)
	var content []string
	for _, record := range records {
		switch record.Kind {
		case jsonl.KindProject, jsonl.KindIssue, jsonl.KindComment, jsonl.KindIssueLabel,
			jsonl.KindLink, jsonl.KindRecurrence, jsonl.KindPurgeLog, jsonl.KindProjectPurgeLog:
		case jsonl.KindEvent:
			var event db.EventExport
			require.NoError(t, json.Unmarshal(record.Data, &event))
			if event.Type == "token.created" || event.Type == "token.revoked" {
				continue
			}
		default:
			continue
		}
		content = append(content, string(record.Kind)+":"+string(record.Data))
	}
	return content
}
