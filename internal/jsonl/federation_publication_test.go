package jsonl_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func publicationSource(t *testing.T, backend string) (db.Storage, db.Project) {
	t.Helper()
	ctx := t.Context()
	var source db.Storage
	if backend == "sqlite" {
		s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
		require.NoError(t, err)
		source = s
	} else {
		dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
		t.Cleanup(cleanup)
		s, err := pgstore.Open(ctx, dsn)
		require.NoError(t, err)
		source = s
	}
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	project, _, err := source.CreateProjectAndEvent(ctx, "example-project", "writer")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Portable issue", Author: "writer"})
	require.NoError(t, err)
	return source, project
}

func capturePublication(t *testing.T, source db.Storage, p db.Project) db.FederationPublication {
	t.Helper()
	publication, err := jsonl.CaptureFederationPublication(t.Context(), source, p.ID, db.FederationPublication{
		OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(),
		ProjectUID: p.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: p.Name,
	})
	require.NoError(t, err)
	return publication
}

func publicationWithRecordKind(t *testing.T, contents []byte, from, to string) []byte {
	t.Helper()
	var envelope map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(contents, &envelope))
	var records []map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(envelope["records"], &records))
	found := false
	for _, record := range records {
		if string(record["kind"]) == `"`+from+`"` {
			record["kind"] = jsontext.Value(`"` + to + `"`)
			found = true
			break
		}
	}
	require.True(t, found)
	var err error
	envelope["records"], err = json.Marshal(records)
	require.NoError(t, err)
	delete(envelope, "digest")
	unsigned, err := json.Marshal(envelope)
	require.NoError(t, err)
	canonical, err := db.CanonicalEventJSON(unsigned)
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	envelope["digest"], err = json.Marshal(hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	updated, err := json.Marshal(envelope)
	require.NoError(t, err)
	return updated
}

// A standalone project can still retain federation authority without a binding.
// Capture must refuse that authority instead of silently dropping its root pin.
func TestFederationPublicationRejectsAttribution(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			public, _, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			require.NoError(t, source.PinRootAuthority(t.Context(), db.RootKeyPin{
				ProjectUID: p.UID, AuthorityUID: source.InstanceUID(),
				KeyID: db.RootPublicKeyID(public), PublicKey: public,
			}))
			_, err = jsonl.CaptureFederationPublication(t.Context(), source, p.ID, db.FederationPublication{
				OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(),
				ProjectUID: p.UID, ProjectName: p.Name, TargetInstanceUID: "01K00000000000000000000006",
			})
			require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
		})
	}
}

func TestFederationPublicationClosedEnvelope(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			publication := capturePublication(t, source, p)
			contents, err := jsonl.EncodeFederationPublication(publication)
			require.NoError(t, err)
			decoded, err := jsonl.DecodeFederationPublication(contents)
			require.NoError(t, err)
			require.Equal(t, publication, decoded)
			encoded, err := jsonl.EncodeFederationPublication(decoded)
			require.NoError(t, err)
			require.Equal(t, contents, encoded)
			for _, r := range decoded.Records {
				require.Contains(t, []string{"project", "issue", "comment", "issue_label", "link", "event"}, r.ImportKind())
			}
			for _, bad := range [][]byte{
				bytes.Replace(contents, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
				bytes.Replace(contents, []byte(`"version":1`), []byte(`"version":2`), 1),
				bytes.Replace(contents, []byte(`"version":1`), []byte(`"version":1,"visibility":"all"`), 1),
				bytes.Replace(contents, []byte(`"title":"Portable issue"`), []byte(`"title":"altered"`), 1),
			} {
				_, err := jsonl.DecodeFederationPublication(bad)
				require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
			}
			for _, kind := range []string{"meta", "future"} {
				bad := publicationWithRecordKind(t, contents, "issue", kind)
				_, err := jsonl.DecodeFederationPublication(bad)
				require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
				require.Contains(t, err.Error(), "unsupported record kind")
			}
		})
	}
}

// Version 1 freezes one typed record shape. A digest over a different shape
// must not be accepted and later fail to re-encode as that same operation.
func TestFederationPublicationRejectsAmbiguousRecordShape(t *testing.T) {
	source, p := publicationSource(t, "sqlite")
	publication := capturePublication(t, source, p)
	encoded, err := jsonl.EncodeFederationPublication(publication)
	require.NoError(t, err)
	var wire map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(encoded, &wire))
	var records []map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(wire["records"], &records))
	for _, record := range records {
		if string(record["kind"]) == `"issue"` {
			var data map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(record["data"], &data))
			delete(data, "body")
			record["data"], err = json.Marshal(data)
			require.NoError(t, err)
		}
	}
	wire["records"], err = json.Marshal(records)
	require.NoError(t, err)
	delete(wire, "digest")
	unsigned, err := json.Marshal(wire)
	require.NoError(t, err)
	canonical, err := db.CanonicalEventJSON(unsigned)
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	wire["digest"], err = json.Marshal(hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	malformed, err := json.Marshal(wire)
	require.NoError(t, err)
	_, err = jsonl.DecodeFederationPublication(malformed)
	require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
}

func TestFederationPublicationOriginalSnapshot(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			snapshot, release, err := source.(interface {
				BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
			}).BeginExportSnapshot(t.Context())
			require.NoError(t, err)
			defer func() { require.NoError(t, release()) }()
			reader := snapshot.(db.FederationPublicationSnapshotReader)
			var original []db.EventExport
			for event, err := range reader.ExportFederationPublicationEvents(t.Context(), p.ID) {
				require.NoError(t, err)
				original = append(original, event)
			}
			_, _, err = source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Later", Author: "writer"})
			require.NoError(t, err)
			var again []db.EventExport
			for event, err := range reader.ExportFederationPublicationEvents(t.Context(), p.ID) {
				require.NoError(t, err)
				again = append(again, event)
			}
			require.Equal(t, original, again, "raw events stay on the established snapshot")
			require.Len(t, original, 2)
		})
	}
}

func TestFederationPublicationCaptureRejectsOriginalCorruption(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			// Owner export recomputes hashes; publication must reject the stored
			// corruption rather than freezing a newly invented hash.
			query := "UPDATE events SET content_hash='" + strings.Repeat("a", 64) + "' WHERE project_id="
			if backend == "sqlite" {
				_, err := source.(*sqlitestore.Store).ExecContext(t.Context(), query+"?", p.ID)
				require.NoError(t, err)
			} else {
				_, err := source.(*pgstore.Store).ExecContext(t.Context(), query+"$1", p.ID)
				require.NoError(t, err)
			}
			_, err := jsonl.CaptureFederationPublication(t.Context(), source, p.ID, db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(), ProjectUID: p.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: p.Name})
			require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
		})
	}
}

func TestFederationPublicationPreservesPayloadBytes(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			var original db.EventExport
			for event, err := range source.(db.FederationPublicationSnapshotReader).ExportFederationPublicationEvents(t.Context(), p.ID) {
				require.NoError(t, err)
				if event.Type == "issue.created" {
					original = event
				}
			}
			payload := "{\n  " + strings.Replace(string(original.Payload[1:len(original.Payload)-1]), `"Portable issue"`, `"\u0050ortable issue"`, 1) + "\n}"
			executor := source.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			query := "UPDATE events SET payload=? WHERE id=?"
			if backend == "postgres" {
				query = "UPDATE events SET payload=$1 WHERE id=$2"
			}
			_, err := executor.ExecContext(t.Context(), query, payload, original.ID)
			require.NoError(t, err)
			publication := capturePublication(t, source, p)
			wire, err := jsonl.EncodeFederationPublication(publication)
			require.NoError(t, err)
			decoded, err := jsonl.DecodeFederationPublication(wire)
			require.NoError(t, err)
			for _, record := range decoded.Records {
				if event, ok := record.(*db.EventExport); ok && event.ID == original.ID {
					require.Equal(t, payload, string(event.Payload))
					require.Equal(t, original.ContentHash, event.ContentHash)
					return
				}
			}
			t.Fatal("original event missing")
		})
	}
}

// A capacity refusal must occur while collecting the source, before advancing
// to later iterators. This uses the native capacity override at a small size.
func TestFederationPublicationCaptureCapacity(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"), db.WithRelayCheckpointCapacity(64))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	p, _, err := source.CreateProjectAndEvent(t.Context(), "example-project", "writer")
	require.NoError(t, err)
	_, err = jsonl.CaptureFederationPublication(t.Context(), source, p.ID, db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(), ProjectUID: p.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: p.Name})
	require.ErrorIs(t, err, db.ErrFederationPublicationTooLarge)
}

type publicationBudgetSource struct{ db.Storage }
type publicationBudgetSnapshot struct {
	db.Storage
	db.FederationPublicationSnapshotReader
}

func (s publicationBudgetSource) BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error) {
	snapshot, release, err := s.Storage.(relaySnapshotter).BeginExportSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	return publicationBudgetSnapshot{Storage: snapshot, FederationPublicationSnapshotReader: snapshot.(db.FederationPublicationSnapshotReader)}, release, nil
}
func (s publicationBudgetSnapshot) ExportFederationPublicationEvents(context.Context, int64) iter.Seq2[db.EventExport, error] {
	return func(yield func(db.EventExport, error) bool) {
		yield(db.EventExport{}, errors.New("capture advanced beyond its exhausted budget"))
	}
}

func TestFederationPublicationCaptureStopsAtBudget(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"), db.WithRelayCheckpointCapacity(4096))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	p, _, err := source.CreateProjectAndEvent(t.Context(), "example-project", "writer")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Large issue", Body: strings.Repeat("x", 8192), Author: "writer"})
	require.NoError(t, err)
	_, err = jsonl.CaptureFederationPublication(t.Context(), publicationBudgetSource{source}, p.ID, db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(), ProjectUID: p.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: p.Name})
	require.ErrorIs(t, err, db.ErrFederationPublicationTooLarge, "later event stream must never be consumed")
}

func TestFederationPublicationHistoricalOutsideReference(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			ctx := t.Context()
			issues, err := source.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
			require.NoError(t, err)
			i := issues[0]
			other, _, err := source.CreateProjectAndEvent(ctx, "other-project", "writer")
			require.NoError(t, err)
			peer, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Outside peer", Author: "writer"})
			require.NoError(t, err)
			ev := db.LinkEventParams{EventType: "issue.linked", EventIssueID: i.ID, FromUID: i.UID, ToUID: peer.UID, FromShortID: i.ShortID, ToShortID: peer.ShortID, Actor: "writer"}
			link, _, err := source.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: i.ID, ToIssueID: peer.ID, Type: "related", Author: "writer"}, ev)
			require.NoError(t, err)
			ev.EventType = "issue.unlinked"
			_, err = source.DeleteLinkAndEvent(ctx, link, ev)
			require.NoError(t, err)
			assertRefused := func() {
				_, err := jsonl.CaptureFederationPublication(ctx, source, p.ID, db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(), ProjectUID: p.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: p.Name})
				require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
			}
			assertRefused()
			_, _, _, err = source.SoftDeleteIssue(ctx, peer.ID, "writer")
			require.NoError(t, err)
			_, err = source.PurgeIssue(ctx, peer.ID, "writer", nil)
			require.NoError(t, err)
			assertRefused()
		})
	}
}

func TestFederationPublicationRichHistory(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, p := publicationSource(t, backend)
			ctx := t.Context()
			issues, err := source.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
			require.NoError(t, err)
			i := issues[0]
			comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: i.ID, Author: "writer", Teammate: "builder", Body: "Original"})
			require.NoError(t, err)
			_, _, err = source.CreateComment(ctx, db.CreateCommentParams{IssueID: i.ID, Author: "reviewer", Teammate: "reviewer-1", Body: "Reply", ReplyToUID: comment.UID, ReplyKind: "reply", ValidateReply: true})
			require.NoError(t, err)
			_, _, _, err = source.EditComment(ctx, db.EditCommentParams{IssueID: i.ID, CommentUID: comment.UID, Actor: "writer", Body: "Edited"})
			require.NoError(t, err)
			capturePublication(t, source, p)
			_, _, err = source.AddLabelAndEvent(ctx, i.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "ready", Actor: "writer"})
			require.NoError(t, err)
			_, _, _, err = source.UpdateOwner(ctx, i.ID, new("worker"), "writer")
			require.NoError(t, err)
			_, _, _, err = source.UpdatePriority(ctx, i.ID, new(int64(2)), "writer")
			require.NoError(t, err)
			capturePublication(t, source, p)
			peer, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Peer", Author: "reviewer"})
			require.NoError(t, err)
			_, _, err = source.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: i.ID, ToIssueID: peer.ID, Type: "related", Author: "writer"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: i.ID, FromUID: i.UID, ToUID: peer.UID, FromShortID: i.ShortID, ToShortID: peer.ShortID, Actor: "writer"})
			require.NoError(t, err)
			capturePublication(t, source, p)
			_, err = source.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: i.ID, Actor: "writer", Patch: map[string]jsontext.Value{"custom": jsontext.Value(`"value"`)}})
			require.NoError(t, err)
			_, err = source.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: p.ID, Actor: "writer", Patch: map[string]jsontext.Value{"custom": jsontext.Value(`"value"`)}})
			require.NoError(t, err)
			capturePublication(t, source, p)
			_, _, _, err = source.CloseIssueWithEvents(ctx, i.ID, "done", "writer", "Finished portable history", []db.Evidence{{Type: "test", Command: "go test ./example"}})
			require.NoError(t, err)
			capturePublication(t, source, p)
			_, _, _, err = source.ReopenIssue(ctx, i.ID, "writer")
			require.NoError(t, err)
			_, _, _, err = source.SoftDeleteIssue(ctx, peer.ID, "writer")
			require.NoError(t, err)
			capturePublication(t, source, p)
			_, _, _, err = source.RestoreIssue(ctx, peer.ID, "writer")
			require.NoError(t, err)
			p, _, _, err = source.RenameProjectAndEvent(ctx, p.ID, "renamed-project", "writer")
			require.NoError(t, err)
			publication := capturePublication(t, source, p)
			for _, r := range publication.Records {
				if e, ok := r.(*db.EventExport); ok && e.Type == "issue.created" {
					require.Equal(t, "example-project", e.ProjectName)
				}
			}
		})
	}
}

func TestFederationPublicationCapturesNativeMutationPayloads(t *testing.T) {
	source, project := publicationSource(t, "sqlite")
	ctx := t.Context()
	issues, err := source.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	issue := issues[0]
	peer, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Related peer", Author: "writer"})
	require.NoError(t, err)

	_, _, _, err = source.UpdateOwner(ctx, issue.ID, new("worker"), "writer")
	require.NoError(t, err)
	title := "Renamed issue"
	clearOwner := ""
	_, err = source.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "writer", Title: &title, Owner: &clearOwner})
	require.NoError(t, err)
	priority := int64(2)
	_, _, _, err = source.UpdatePriority(ctx, issue.ID, &priority, "writer")
	require.NoError(t, err)
	_, _, _, err = source.UpdatePriority(ctx, issue.ID, nil, "writer")
	require.NoError(t, err)
	_, err = source.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "writer", AddRelated: []int64{peer.ID}})
	require.NoError(t, err)

	publication, err := jsonl.CaptureFederationPublication(ctx, source, project.ID, db.FederationPublication{
		OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(),
		ProjectUID: project.UID, TargetInstanceUID: "01K00000000000000000000006", ProjectName: project.Name,
	})
	require.NoError(t, err)
	encoded, err := jsonl.EncodeFederationPublication(publication)
	require.NoError(t, err)
	decoded, err := jsonl.DecodeFederationPublication(encoded)
	require.NoError(t, err)

	var sawIssueEdit, sawPriorityClear, sawRelated bool
	var beforePayloads, afterPayloads []string
	for _, record := range publication.Records {
		if event, ok := record.(*db.EventExport); ok {
			beforePayloads = append(beforePayloads, string(event.Payload))
			switch event.Type {
			case "issue.updated":
				var payload map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(event.Payload, &payload))
				if payload["old_title"] != nil {
					require.JSONEq(t, `"Portable issue"`, string(payload["old_title"]))
					require.Equal(t, jsontext.Value("null"), payload["owner"])
					require.JSONEq(t, `"worker"`, string(payload["old_owner"]))
					sawIssueEdit = true
				}
			case "issue.priority_cleared":
				var payload map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(event.Payload, &payload))
				require.JSONEq(t, `2`, string(payload["old_priority"]))
				sawPriorityClear = true
			case "issue.links_changed":
				var payload map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(event.Payload, &payload))
				var refs, uids []string
				require.NoError(t, json.Unmarshal(payload["related_added"], &refs))
				require.NoError(t, json.Unmarshal(payload["related_added_uids"], &uids))
				require.Equal(t, []string{peer.ShortID}, refs)
				require.Equal(t, []string{peer.UID}, uids)
				sawRelated = true
			}
		}
	}
	for _, record := range decoded.Records {
		if event, ok := record.(*db.EventExport); ok {
			afterPayloads = append(afterPayloads, string(event.Payload))
		}
	}
	require.Equal(t, beforePayloads, afterPayloads, "publication round trips retain each native payload byte-for-byte")
	require.True(t, sawIssueEdit)
	require.True(t, sawPriorityClear)
	require.True(t, sawRelated)
}

func TestFederationPublicationCapturesNativeNullOwnerPayloads(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, project := publicationSource(t, backend)
			ctx := t.Context()
			issues, err := source.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 1)
			issue := issues[0]

			owner := "worker"
			_, _, changed, err := source.UpdateOwner(ctx, issue.ID, &owner, "writer")
			require.NoError(t, err)
			require.True(t, changed)
			_, unassigned, changed, err := source.UnassignOwner(ctx, issue.ID, "writer", &owner)
			require.NoError(t, err)
			require.True(t, changed)
			require.NotNil(t, unassigned)

			base := time.Now().UTC().Truncate(time.Second)
			_, err = source.ClaimOwner(ctx, db.ClaimOwnerParams{
				IssueID: issue.ID, Actor: owner, TTL: time.Minute, Now: base,
			})
			require.NoError(t, err)
			expired, err := source.ExpireAssignments(ctx, db.ExpireAssignmentsParams{
				ProjectID: project.ID, Now: base.Add(time.Minute),
			})
			require.NoError(t, err)
			require.Len(t, expired, 1)
			require.Equal(t, "issue.assignment_expired", expired[0].Type)

			publication := capturePublication(t, source, project)
			sawUnassigned, sawExpired := false, false
			for _, record := range publication.Records {
				event, ok := record.(*db.EventExport)
				if !ok {
					continue
				}
				if event.Type != "issue.unassigned" && event.Type != "issue.assignment_expired" {
					continue
				}
				payload := db.PayloadMap(event.Payload)
				require.Equal(t, jsontext.Value("null"), payload["owner"], event.Type)
				if event.Type == "issue.unassigned" {
					sawUnassigned = true
				} else {
					sawExpired = true
				}
			}
			require.True(t, sawUnassigned)
			require.True(t, sawExpired)
		})
	}
}

func FuzzFederationPublicationEnvelope(f *testing.F) {
	// A retained introduction is deliberately formatted without json.Marshal.
	// Successful mutations must retain its bytes, content and digest exactly.
	const stamp = "2026-10-20T12:00:00.000Z"
	const project = "01K00000000000000000000001"
	const issue = "01K0000000000000000000AB34"
	event := &db.EventExport{ID: 3, UID: "01K00000000000000000000003", OriginInstanceUID: "01K00000000000000000000004", ProjectID: 1, ProjectUID: project, ProjectName: "example-project", IssueUID: new(issue), Type: "issue.snapshot", Actor: "writer", HLCPhysicalMS: 1792497600000, CreatedAt: stamp, Payload: []byte("{\n" + `"uid":"` + issue + `","short_id":"ab34","title":"Portable issue","body":"","author":"writer","status":"open","created_at":"` + stamp + `","metadata":{}}`)}
	if err := db.PrepareReplayEvent(event, project, event.ProjectName, true); err != nil {
		f.Fatal(err)
	}
	seed := db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: event.OriginInstanceUID, ProjectUID: project, TargetInstanceUID: "01K00000000000000000000006", ProjectName: event.ProjectName, Records: []db.ImportRecord{
		&db.ProjectExport{ID: 1, UID: project, Name: event.ProjectName, CreatedAt: stamp, Metadata: []byte(`{}`), Revision: 1},
		&db.IssueExport{ID: 2, UID: issue, ProjectID: 1, ShortID: "ab34", Title: "Portable issue", Author: "writer", Status: "open", CreatedAt: stamp, UpdatedAt: stamp, Metadata: []byte(`{}`), Revision: 1}, event,
	}}
	accepted, err := jsonl.EncodeFederationPublication(seed)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(accepted)
	for _, pair := range [][2]string{{project, "invalid-uid"}, {event.ContentHash, strings.Repeat("0", 64)}, {`"issue.snapshot"`, `"issue.assigned"`}, {`\"uid\"`, `\"future_uid\"`}} {
		f.Add(bytes.Replace(accepted, []byte(pair[0]), []byte(pair[1]), 1))
	}
	// Empty/unknown/duplicate-object seeds exercise fail-closed decoding.
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Add([]byte(`{"version":1,"records":[{"kind":"future","data":{}}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip("bounded materialization")
		}
		// The outer checksum is public, so refresh it for syntactically valid
		// mutations to reach the typed/content validator. Never repair an
		// original event's hash or payload to make a mutation acceptable.
		var wire map[string]jsontext.Value
		if json.Unmarshal(raw, &wire) == nil && wire != nil {
			delete(wire, "digest")
			unsigned, err := json.Marshal(wire)
			require.NoError(t, err)
			canonical, err := db.CanonicalEventJSON(unsigned)
			require.NoError(t, err)
			sum := sha256.Sum256(canonical)
			wire["digest"], err = json.Marshal(hex.EncodeToString(sum[:]))
			require.NoError(t, err)
			raw, err = json.Marshal(wire)
			require.NoError(t, err)
		}
		publication, err := jsonl.DecodeFederationPublication(raw)
		if err != nil {
			return
		}
		encoded, err := jsonl.EncodeFederationPublication(publication)
		require.NoError(t, err)
		again, err := jsonl.DecodeFederationPublication(encoded)
		require.NoError(t, err)
		require.Equal(t, publication, again)
		// Returned records own their raw bytes, even when the caller reuses
		// the network buffer after decode.
		before, err := json.Marshal(publication.Records)
		require.NoError(t, err)
		clear(raw)
		after, err := json.Marshal(publication.Records)
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
}
