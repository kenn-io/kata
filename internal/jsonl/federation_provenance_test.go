package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R5: creation proof survives edits, compacted source history and owner restore.
// Ordinary legacy rows must never acquire verified provenance during replay.
func TestProvenanceSurvivesSnapshotAndRestore(t *testing.T) {
	for _, cutover := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "legacy_export_path"}[cutover], func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project, err := source.CreateProject(ctx, "shared-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, source.PinRootAuthority(ctx, pin))
			grant, err := source.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "00000000000000000000000002", Actor: "company-member", Capabilities: "pull,push", Token: "backup-grant"})
			require.NoError(t, err)
			issue, creation, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original creation", Author: "assistant"})
			require.NoError(t, err)
			comment, commentEvent, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Teammate: "reviewer", Body: "Original comment"})
			require.NoError(t, err)
			expected := map[string]db.AttributionReceipt{}
			for _, event := range []db.Event{creation, commentEvent} {
				remote := db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, Payload: []byte(event.Payload), HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, ContentHash: event.ContentHash, CreatedAt: event.CreatedAt}
				receipt, e := source.RecordRootAttribution(ctx, grant.Enrollment.ID, remote, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private})
				require.NoError(t, e)
				expected[event.UID] = receipt
			}
			// Rotation retains the old verification key while replacing only the active pin.
			replacementPublic, _, e := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, e)
			replacement := pin
			replacement.KeyID, replacement.PublicKey = db.RootPublicKeyID(replacementPublic), replacementPublic
			_, err = source.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=?`, project.UID)
			require.NoError(t, err)
			_, err = source.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?,?,1)`, project.UID, pin.AuthorityUID, replacement.KeyID, base64.StdEncoding.EncodeToString(replacement.PublicKey))
			require.NoError(t, err)
			title := "Edited task"
			_, _, _, err = source.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Actor: "editor", Title: &title})
			require.NoError(t, err)
			_, _, _, err = source.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "comment-assistant", Body: "Edited comment"})
			require.NoError(t, err)
			legacy, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Legacy", Author: "legacy-agent"})
			require.NoError(t, err)
			// Test-local compaction: no proof FK may depend on retaining source events.
			_, err = source.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, project.ID)
			require.NoError(t, err)
			cursor, err := source.UIEventCursor(ctx)
			require.NoError(t, err)
			var backup bytes.Buffer
			if cutover {
				err = exportForCutover(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			}
			require.NoError(t, err)
			for _, kind := range []string{"federation_root_key", "federation_event_provenance", "federation_entity_provenance"} {
				require.Contains(t, backup.String(), `"kind":"`+kind+`"`)
			}
			require.NotContains(t, backup.String(), "private_key")
			require.False(t, strings.Contains(backup.String(), base64.StdEncoding.EncodeToString(private.Seed())), "signing material must never enter an ordinary backup")
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			postgres, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = postgres.Close() })
			require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), postgres))
			pgCursor, err := postgres.UIEventCursor(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, pgCursor, "owner restore retains late-proof reset authority after compaction")
			backup.Reset()
			require.NoError(t, Export(ctx, postgres, &backup, ExportOptions{IncludeDeleted: true}))
			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
			restoredCursor, err := target.UIEventCursor(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, restoredCursor, "cross-backend restore keeps durable UI validators")
			// Corrupt a signed account without re-signing. Rejection must preserve an
			// initialized target, rather than clearing it before signature validation.
			tampered := bytes.ReplaceAll(backup.Bytes(), []byte(`"accountable_actor":"company-member"`), []byte(`"accountable_actor":"impostor"`))
			require.NotEqual(t, backup.Bytes(), tampered)
			for _, store := range []db.Storage{postgres, target} {
				require.Error(t, Import(ctx, bytes.NewReader(tampered), store))
				restoredPin, e := store.RootAuthority(ctx, project.UID)
				require.NoError(t, e)
				require.Equal(t, replacement, restoredPin)
				for _, entry := range []struct{ kind, entity, event string }{{"issue", issue.UID, creation.UID}, {"comment", comment.UID, commentEvent.UID}} {
					receipt, e := store.EntityAttribution(ctx, project.UID, entry.kind, entry.entity)
					require.NoError(t, e)
					require.Equal(t, expected[entry.event], receipt)
					require.NoError(t, db.VerifyRootReceipt(pin, receipt))
				}
				_, e = store.EntityAttribution(ctx, project.UID, "issue", legacy.UID)
				require.ErrorIs(t, e, db.ErrNotFound)
				page, e := store.AttributionReceiptsAfter(ctx, project.UID, 1, 0, 10)
				require.NoError(t, e)
				require.Len(t, page, 2)
				page, e = store.AttributionReceiptsAfter(ctx, project.UID, 1, page[1].Sequence, 10)
				require.NoError(t, e)
				require.Empty(t, page)
				restored, e := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, e)
				require.Equal(t, "assistant", restored.Author)
				require.Equal(t, title, restored.Title)
			}
		})
	}
}
