package jsonl

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

// A full-owner export may refuse live-only filtering before writing, or retain
// the deleted rows required by its close proofs. It cannot report success with
// a snapshot that its own restore rejects or that silently drops those proofs.
func TestScopedBackupFilteredCloseDependencies(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			for _, deleted := range []string{"subject", "peer", "unrelated", "archived_peer_project"} {
				t.Run(deleted, func(t *testing.T) {
					ctx := t.Context()
					source := artifactBackupStore(t, backend)
					f := dbtest.NewScopedCaptureFixture(t, source)
					subject, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
						ProjectID: f.Projects[0].ID, Title: "Closed work", Author: "member",
					})
					require.NoError(t, err)
					peerProject := f.Projects[1]
					if deleted == "archived_peer_project" {
						peerProject, err = source.CreateProject(ctx, "archive-peer-project")
						require.NoError(t, err)
						_, err := source.FederationBindingByProject(ctx, peerProject.ID)
						require.ErrorIs(t, err, db.ErrNotFound, "archive an actual unbound project without changing any binding")
					}
					peer, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
						ProjectID: peerProject.ID, Title: "Disposition peer", Author: "member",
					})
					require.NoError(t, err)
					comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{
						IssueID: subject.ID, Author: "member", Body: "Retained close context",
					})
					require.NoError(t, err)
					value := &db.ScopedCloseValue{
						Kind: "duplicate", PeerProjectUID: peer.ProjectUID, PeerIssueUID: peer.UID,
					}
					_, closeEvents, changed, err := source.CloseIssueGuarded(ctx, db.CloseIssueParams{
						IssueID: subject.ID, Reason: "duplicate", Actor: "member", CloseValue: value,
					})
					require.NoError(t, err)
					require.True(t, changed)
					require.NotEmpty(t, closeEvents)
					deleteID := subject.ID
					if deleted == "peer" {
						deleteID = peer.ID
					} else if deleted == "unrelated" {
						project, err := source.CreateProject(ctx, "unrelated-project")
						require.NoError(t, err)
						issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Unrelated work", Author: "member"})
						require.NoError(t, err)
						deleteID = issue.ID
					}
					if deleted == "archived_peer_project" {
						archived, event, err := source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: peerProject.ID, Actor: "member", Force: true})
						require.NoError(t, err)
						require.NotNil(t, event)
						require.NotNil(t, archived.DeletedAt)
						currentPeer, err := source.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
						require.NoError(t, err)
						require.Nil(t, currentPeer.DeletedAt, "project archive does not delete the peer issue")
						projectsPresent, issuesPresent := false, false
						for project, err := range source.ExportProjects(ctx, db.ExportFilter{IncludeDeleted: false}) {
							require.NoError(t, err)
							projectsPresent = projectsPresent || project.UID == peerProject.UID
						}
						for issue, err := range source.ExportIssues(ctx, db.ExportFilter{IncludeDeleted: false}) {
							require.NoError(t, err)
							issuesPresent = issuesPresent || issue.UID == peer.UID
						}
						require.True(t, projectsPresent, "native project exporter actually retains archived projects")
						require.True(t, issuesPresent, "native live-only issue exporter actually retains this live peer")
					} else {
						_, event, changed, err := source.SoftDeleteIssue(ctx, deleteID, "member")
						require.NoError(t, err)
						require.True(t, changed)
						require.NotNil(t, event)
					}
					proof := dbtest.ScopedCurrentClose(t, source, subject.UID)
					require.Equal(t, closeEvents[0].UID, proof.StatusIntentUID)
					require.Equal(t, value, proof.Value)

					// Establish the actual native graph's full JSONL restore positive
					// before exercising the filtered exporter. No rows are fabricated.
					var full bytes.Buffer
					require.NoError(t, Export(ctx, source, &full, ExportOptions{IncludeDeleted: true}))
					target := artifactBackupStore(t, backend)
					require.NoError(t, Import(ctx, bytes.NewReader(full.Bytes()), target), "unfiltered owner backup must restore first")
					require.Equal(t, proof, dbtest.ScopedCurrentClose(t, target, subject.UID))
					comments, err := target.CommentsByIssue(ctx, subject.ID)
					require.NoError(t, err)
					require.Len(t, comments, 1)
					require.Equal(t, comment.UID, comments[0].UID)
					require.Equal(t, comment.Body, comments[0].Body)

					var filtered bytes.Buffer
					err = Export(ctx, source, &filtered, ExportOptions{IncludeDeleted: false})
					if deleted == "unrelated" || deleted == "archived_peer_project" {
						require.NoError(t, err, "filtering must succeed when native exporters omit no close-proof dependency")
					}
					if err != nil {
						require.Zero(t, filtered.Len(), "unsupported owner filtering must refuse before writing incomplete output")
					} else {
						require.NoError(t, Import(ctx, bytes.NewReader(filtered.Bytes()), target), "successful owner export must retain close-proof dependency closure")
						require.Equal(t, proof, dbtest.ScopedCurrentClose(t, target, subject.UID), "filtering cannot discard retained current proof to make restore pass")
						for _, uid := range []string{subject.UID, peer.UID} {
							original, err := source.IssueByUID(ctx, uid, db.IncludeDeletedYes)
							require.NoError(t, err)
							restored, err := target.IssueByUID(ctx, uid, db.IncludeDeletedYes)
							require.NoError(t, err, "close-proof subject and peer must remain available to restore")
							require.Equal(t, original, restored)
						}
					}
					var after bytes.Buffer
					require.NoError(t, Export(ctx, source, &after, ExportOptions{IncludeDeleted: true}))
					require.Equal(t, full.Bytes(), after.Bytes(), "export and restore must not alter the source's full owner state")
				})
			}
		})
	}
}
