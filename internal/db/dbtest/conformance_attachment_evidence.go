package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// A document close validates and freezes the current reference in its mutation
// transaction, using the same contract on SQLite and PostgreSQL.
func checkAttachmentCloseEvidence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Deliver final report", "operator", nil)
	if err != nil {
		return err
	}
	const id = "01K7E222222222222222222222"
	evidence := []db.Evidence{{Type: "attachment", AttachmentID: id, Attachment: &metadata.Attachment{Name: "forged.pdf"}}}
	for _, raw := range []jsontext.Value{nil, jsontext.Value(`null`), jsontext.Value(`{"name":"bad.pdf","media_type":"application/pdf","size":-1,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}`)} {
		_, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + id: raw}})
		require.NoError(t, err)
		before, err := store.IssueByID(ctx, f.Issue.ID)
		require.NoError(t, err)
		cursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		_, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: evidence})
		require.ErrorIs(t, err, db.ErrAttachmentEvidence)
		require.False(t, changed)
		require.Empty(t, events)
		after, err := store.IssueByID(ctx, f.Issue.ID)
		require.NoError(t, err)
		require.Equal(t, before, after)
		next, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, next)
	}
	entry := metadata.Attachment{Name: "final-report.pdf", MediaType: "application/pdf", Size: 12, DocbankVersion: uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")}
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + id: raw}})
	require.NoError(t, err)

	const secondID = "01K7E333333333333333333333"
	multiple := append(append([]db.Evidence{}, evidence...), db.Evidence{Type: "attachment", AttachmentID: secondID})
	before, err := store.IssueByID(ctx, f.Issue.ID)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, missingEvents, missingChanged, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: multiple})
	require.ErrorIs(t, err, db.ErrAttachmentEvidence)
	require.ErrorContains(t, err, "evidence[1]")
	require.False(t, missingChanged)
	require.Empty(t, missingEvents)
	after, err := store.IssueByID(ctx, f.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	next, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, next)
	secondEntry := metadata.Attachment{Name: "summary.txt", MediaType: "text/plain", Size: 34, DocbankVersion: uuid.MustParse("6f1c2e8a-3333-4333-8333-333333333333")}
	secondRaw, err := json.Marshal(secondEntry)
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + secondID: secondRaw}})
	require.NoError(t, err)
	closed, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: multiple})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "closed", closed.Status)
	require.NotEmpty(t, events)
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Len(t, payload.Evidence, 2)
	require.Equal(t, secondID, payload.Evidence[1].AttachmentID)
	require.Equal(t, &secondEntry, payload.Evidence[1].Attachment)
	require.Equal(t, id, payload.Evidence[0].AttachmentID)
	require.Equal(t, &entry, payload.Evidence[0].Attachment)
	require.Equal(t, "forged.pdf", evidence[0].Attachment.Name, "server enrichment must not modify caller evidence")
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + id: nil}})
	require.NoError(t, err)
	saved, err := store.EventsByUIDs(ctx, f.Project.ID, []string{events[0].UID})
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.Equal(t, events[0].Payload, saved[0].Payload, "removal cannot rewrite historical descriptor")
	_, _, changed, err = store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: evidence})
	require.NoError(t, err)
	require.False(t, changed, "already closed remains an idempotent no-op")
	return nil
}
