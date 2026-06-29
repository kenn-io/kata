package sqlitestore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestImportBatch_CreatesIssueCommentsLabelsLinks(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: p.ID,
		Source:    "beads",
		Actor:     "importer",
		Items: []db.ImportItem{
			{ExternalID: "blocker", Title: "Blocker", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Labels: []string{"source:beads", "beads-id:blocker"}, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "blocked"}}},
			{ExternalID: "blocked", Title: "Blocked", Body: "body", Author: "bob", Status: "closed", ClosedReason: strPtr("done"), CreatedAt: t1, UpdatedAt: t2, ClosedAt: &t2, Labels: []string{"source:beads", "beads-id:blocked"}, Comments: []db.ImportComment{{ExternalID: "c1", Author: "bob", Body: "note", CreatedAt: t2}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Created)
	assert.Equal(t, 1, res.Comments)
	assert.Equal(t, 1, res.Links)
	assert.NotEmpty(t, events)

	blockedMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "blocked")
	require.NoError(t, err)
	require.NotNil(t, blockedMap.IssueID)
	blocked, err := d.IssueByID(ctx, *blockedMap.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "closed", blocked.Status)
	assert.True(t, blocked.CreatedAt.Equal(t1))
	assert.True(t, blocked.UpdatedAt.Equal(t2))
	require.NotNil(t, blocked.ClosedAt)
	assert.True(t, blocked.ClosedAt.Equal(t2))

	comments := commentsForIssue(ctx, t, d, blocked.ID)
	require.Len(t, comments, 1)
	assert.Equal(t, "note", comments[0].Body)
	assert.True(t, comments[0].CreatedAt.Equal(t2))
	commentMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "comment", "c1")
	require.NoError(t, err)
	require.NotNil(t, commentMap.CommentID)
	assert.Equal(t, comments[0].ID, *commentMap.CommentID)

	var createdEvent *db.Event
	for i := range events {
		if events[i].Type != "issue.created" {
			continue
		}
		payload := unmarshalPayload[struct {
			ExternalID string `json:"external_id"`
		}](t, events[i].Payload)
		if payload.ExternalID == "blocked" {
			createdEvent = &events[i]
			break
		}
	}
	require.NotNil(t, createdEvent)
	createdPayload := unmarshalPayload[struct {
		UID          string   `json:"uid"`
		ShortID      string   `json:"short_id"`
		Title        string   `json:"title"`
		Body         string   `json:"body"`
		Author       string   `json:"author"`
		Status       string   `json:"status"`
		ClosedReason *string  `json:"closed_reason"`
		ClosedAt     *string  `json:"closed_at"`
		Labels       []string `json:"labels"`
		Source       string   `json:"source"`
		ExternalID   string   `json:"external_id"`
		UpdatedAt    string   `json:"updated_at"`
	}](t, createdEvent.Payload)
	assert.Equal(t, blocked.UID, createdPayload.UID)
	assert.Equal(t, blocked.ShortID, createdPayload.ShortID)
	assert.Equal(t, "Blocked", createdPayload.Title)
	assert.Equal(t, "body", createdPayload.Body)
	assert.Equal(t, "bob", createdPayload.Author)
	assert.Equal(t, "closed", createdPayload.Status)
	require.NotNil(t, createdPayload.ClosedReason)
	assert.Equal(t, "done", *createdPayload.ClosedReason)
	require.NotNil(t, createdPayload.ClosedAt)
	assert.Equal(t, "2026-05-01T11:00:00.000Z", *createdPayload.ClosedAt)
	assert.Equal(t, "beads", createdPayload.Source)
	assert.Equal(t, "blocked", createdPayload.ExternalID)
	assert.Equal(t, "2026-05-01T11:00:00.000Z", createdPayload.UpdatedAt)
	assert.Empty(t, createdPayload.Labels, "import-created issue payload should not duplicate label events")

	var commentEvent *db.Event
	for i := range events {
		if events[i].Type == "issue.commented" {
			commentEvent = &events[i]
			break
		}
	}
	require.NotNil(t, commentEvent)
	commentPayload := unmarshalPayload[struct {
		CommentUID        string `json:"comment_uid"`
		Author            string `json:"author"`
		Body              string `json:"body"`
		CreatedAt         string `json:"created_at"`
		Source            string `json:"source"`
		ExternalID        string `json:"external_id"`
		CommentExternalID string `json:"comment_external_id"`
	}](t, commentEvent.Payload)
	assert.Equal(t, comments[0].UID, commentPayload.CommentUID)
	assert.Equal(t, "bob", commentPayload.Author)
	assert.Equal(t, "note", commentPayload.Body)
	assert.Equal(t, "2026-05-01T11:00:00.000Z", commentPayload.CreatedAt)
	assert.Equal(t, "beads", commentPayload.Source)
	assert.Equal(t, "blocked", commentPayload.ExternalID)
	assert.Equal(t, "c1", commentPayload.CommentExternalID)

	labels := labelsForIssue(ctx, t, d, blocked.ID)
	require.Len(t, labels, 2)
	assert.Equal(t, "beads-id:blocked", labels[0].Label)
	assert.True(t, labels[0].CreatedAt.Equal(t1))
	assert.Equal(t, "source:beads", labels[1].Label)
	assert.True(t, labels[1].CreatedAt.Equal(t1))
	var labelEvent *db.Event
	for i := range events {
		if events[i].Type == "issue.labeled" && events[i].IssueID != nil && *events[i].IssueID == blocked.ID {
			labelEvent = &events[i]
			break
		}
	}
	require.NotNil(t, labelEvent)
	labelPayload := unmarshalPayload[struct {
		IssueUID string `json:"issue_uid"`
		Label    string `json:"label"`
	}](t, labelEvent.Payload)
	assert.Equal(t, blocked.UID, labelPayload.IssueUID)
	assert.NotEmpty(t, labelPayload.Label)

	blockerMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "blocker")
	require.NoError(t, err)
	require.NotNil(t, blockerMap.IssueID)
	blocker, err := d.IssueByID(ctx, *blockerMap.IssueID)
	require.NoError(t, err)
	links := linksForIssue(ctx, t, d, *blockerMap.IssueID)
	require.Len(t, links, 1)
	assert.Equal(t, *blockerMap.IssueID, links[0].FromIssueID)
	assert.Equal(t, blocked.ID, links[0].ToIssueID)
	assert.Equal(t, "blocks", links[0].Type)
	assert.True(t, links[0].CreatedAt.Equal(t1))
	linkMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "link", "blocker:blocks:blocked")
	require.NoError(t, err)
	require.NotNil(t, linkMap.LinkID)
	assert.Equal(t, links[0].ID, *linkMap.LinkID)

	var linkEvent *db.Event
	for i := range events {
		if events[i].Type == "issue.linked" {
			linkEvent = &events[i]
			break
		}
	}
	require.NotNil(t, linkEvent)
	payload := unmarshalPayload[struct {
		LinkID      int64  `json:"link_id"`
		Type        string `json:"type"`
		FromShortID string `json:"from_short_id"`
		FromUID     string `json:"from_uid"`
		ToShortID   string `json:"to_short_id"`
		ToUID       string `json:"to_uid"`
	}](t, linkEvent.Payload)
	assert.Equal(t, links[0].ID, payload.LinkID)
	assert.Equal(t, "blocks", payload.Type)
	assert.Equal(t, blocker.ShortID, payload.FromShortID)
	assert.Equal(t, blocker.UID, payload.FromUID,
		"import link payload must carry from_uid so SSE consumers key on stable identity")
	assert.Equal(t, blocked.ShortID, payload.ToShortID)
	assert.Equal(t, blocked.UID, payload.ToUID,
		"import link payload must carry to_uid so SSE consumers key on stable identity")
}

func TestImportBatch_BoundFederationActorOverridesImportedAuthors(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestSpokeFederationBindingWithPushActor(ctx, t, d, p, true, true, "wesm")
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: p.ID,
		Source:    "beads",
		Actor:     "importer",
		Items: []db.ImportItem{{
			ExternalID: "external-1",
			Title:      "Imported",
			Body:       "body",
			Author:     "external-author",
			Status:     "open",
			CreatedAt:  t1,
			UpdatedAt:  t1,
			Comments: []db.ImportComment{{
				ExternalID: "comment-1",
				Author:     "external-commenter",
				Body:       "note",
				CreatedAt:  t2,
			}},
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)
	assert.Equal(t, 1, res.Comments)
	mapping, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "external-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	issue, err := d.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "wesm", issue.Author)
	comments := commentsForIssue(ctx, t, d, issue.ID)
	require.Len(t, comments, 1)
	assert.Equal(t, "wesm", comments[0].Author)

	var createdEvent, commentEvent *db.Event
	for i := range events {
		switch events[i].Type {
		case "issue.created":
			createdEvent = &events[i]
		case "issue.commented":
			commentEvent = &events[i]
		}
	}
	require.NotNil(t, createdEvent)
	assert.Equal(t, "wesm", createdEvent.Actor)
	createdPayload := unmarshalPayload[struct {
		Author string `json:"author"`
	}](t, createdEvent.Payload)
	assert.Equal(t, "wesm", createdPayload.Author)

	require.NotNil(t, commentEvent)
	assert.Equal(t, "wesm", commentEvent.Actor)
	commentPayload := unmarshalPayload[struct {
		Author string `json:"author"`
	}](t, commentEvent.Payload)
	assert.Equal(t, "wesm", commentPayload.Author)
}

func TestImportBatch_RelatedLinkEventPayloadKeepsImportDirectionWhenStorageCanonicalizes(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: p.ID,
		Source:    "beads",
		Actor:     "importer",
		Items: []db.ImportItem{
			{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: ts, UpdatedAt: ts},
			{ExternalID: "a", Title: "A", Body: "body", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts, Links: []db.ImportLink{{Type: "related", TargetExternalID: "b"}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Links)

	aMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	require.NotNil(t, aMap.IssueID)
	bMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "b")
	require.NoError(t, err)
	require.NotNil(t, bMap.IssueID)
	a, err := d.IssueByID(ctx, *aMap.IssueID)
	require.NoError(t, err)
	b, err := d.IssueByID(ctx, *bMap.IssueID)
	require.NoError(t, err)
	require.Greater(t, a.ID, b.ID)

	links := linksForIssue(ctx, t, d, a.ID)
	require.Len(t, links, 1)
	assert.Equal(t, b.ID, links[0].FromIssueID)
	assert.Equal(t, a.ID, links[0].ToIssueID)

	var linkEvent *db.Event
	for i := range events {
		if events[i].Type == "issue.linked" {
			linkEvent = &events[i]
			break
		}
	}
	require.NotNil(t, linkEvent)
	payload := unmarshalPayload[struct {
		LinkID      int64  `json:"link_id"`
		Type        string `json:"type"`
		FromShortID string `json:"from_short_id"`
		FromUID     string `json:"from_uid"`
		ToShortID   string `json:"to_short_id"`
		ToUID       string `json:"to_uid"`
	}](t, linkEvent.Payload)
	assert.Equal(t, links[0].ID, payload.LinkID)
	assert.Equal(t, "related", payload.Type)
	// Event attribution is "a created the link"; canonicalization may swap
	// storage endpoints but the payload's from/to follows the URL-issue POV.
	assert.Equal(t, a.ShortID, payload.FromShortID)
	assert.Equal(t, a.UID, payload.FromUID)
	assert.Equal(t, b.ShortID, payload.ToShortID)
	assert.Equal(t, b.UID, payload.ToUID)
}

func TestImportBatch_ReimportSourceNewerUpdatesFieldsAndTimestamp(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	older := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "old", Body: "old body", Author: "alice", Status: "open", CreatedAt: older, UpdatedAt: older}}})
	require.NoError(t, err)
	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "new", Body: "new body", Author: "alice", Status: "closed", ClosedReason: strPtr("done"), CreatedAt: older, UpdatedAt: newer, ClosedAt: &newer}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)

	m, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	issue, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "new", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedAt)
	assert.True(t, issue.ClosedAt.Equal(newer))
	assert.True(t, issue.UpdatedAt.Equal(newer))
	require.Len(t, events, 1)
	payload := unmarshalPayload[struct {
		UpdatedAt string `json:"updated_at"`
		Status    string `json:"status"`
	}](t, events[0].Payload)
	assert.Equal(t, "2026-05-02T10:00:00.000Z", payload.UpdatedAt)
	assert.Equal(t, "closed", payload.Status)
}

func TestImportBatch_LocalNewerIssueUnchangedButMissingCommentsMerge(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	older := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "issue", Body: "body", Author: "alice", Status: "open", CreatedAt: older, UpdatedAt: older}}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	localTitle := "local wins"
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: *m.IssueID, Title: &localTitle, Actor: "local"})
	require.NoError(t, err)

	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "stale", Body: "stale body", Author: "alice", Status: "open", CreatedAt: older, UpdatedAt: older, Comments: []db.ImportComment{{ExternalID: "c1", Author: "alice", Body: "missing", CreatedAt: older}}}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Unchanged)
	assert.Equal(t, 1, res.Comments)

	after, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "local wins", after.Title)
	comments := commentsForIssue(ctx, t, d, *m.IssueID)
	require.Len(t, comments, 1)
	assert.Equal(t, "missing", comments[0].Body)

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "stale", Body: "stale body", Author: "alice", Status: "open", CreatedAt: older, UpdatedAt: older, Comments: []db.ImportComment{{ExternalID: "c1", Author: "alice", Body: "missing", CreatedAt: older}}}}})
	require.NoError(t, err)
	comments = commentsForIssue(ctx, t, d, *m.IssueID)
	assert.Len(t, comments, 1)
}

func TestImportBatch_SameSourceVersionCanCorrectOwnedPresentationTitle(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	sourceTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{{ExternalID: "issue-id:101", Title: "Original title", Body: "body", Author: "alice", Status: "open", CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime}}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{{ExternalID: "issue-id:101", Title: "[GitHub #1] Original title", Body: "body", Author: "alice", Status: "open", CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)
	require.Len(t, events, 1)
	assert.Equal(t, "issue.updated", events[0].Type)
	payload := unmarshalPayload[struct {
		Title    string `json:"title"`
		OldTitle string `json:"old_title"`
	}](t, events[0].Payload)
	assert.Equal(t, "[GitHub #1] Original title", payload.Title)
	assert.Equal(t, "Original title", payload.OldTitle)

	after, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "[GitHub #1] Original title", after.Title)
	assert.True(t, after.UpdatedAt.Equal(sourceTime))
}

func TestImportBatch_SameSourceVersionTitleCorrectionDoesNotClobberLocalEdit(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	sourceTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{{ExternalID: "issue-id:101", Title: "Original title", Body: "body", Author: "alice", Status: "open", CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime}}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)
	localTitle := "local title"
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: *m.IssueID, Title: &localTitle, Actor: "local"})
	require.NoError(t, err)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{{ExternalID: "issue-id:101", Title: "[GitHub #1] Original title", Body: "body", Author: "alice", Status: "open", CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Unchanged)
	assert.Empty(t, events)

	after, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.Equal(t, "local title", after.Title)
	assert.True(t, after.UpdatedAt.After(sourceTime))
}

func TestImportBatch_SourceOwnedLabelsLinksReconcileLocalRemain(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Labels: []string{"source:beads", "old"}, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "b"}}},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "c", Title: "C", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	aMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	bMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "b")
	require.NoError(t, err)
	cMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "c")
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, *aMap.IssueID, "local", "local")
	require.NoError(t, err)
	localLink := makeLink(ctx, t, d, *aMap.IssueID, *cMap.IssueID, "related")

	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A2", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, Labels: []string{"source:beads", "new"}, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "c"}}},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "c", Title: "C", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)

	labels := labelsForIssue(ctx, t, d, *aMap.IssueID)
	assert.Equal(t, []string{"local", "new", "source:beads"}, labelNames(labels))
	_, err = d.ImportMappingBySource(ctx, p.ID, "beads", "label", "a:label:old")
	assert.ErrorIs(t, err, db.ErrNotFound)

	links := linksForIssue(ctx, t, d, *aMap.IssueID)
	require.Len(t, links, 2)
	assert.Contains(t, linkTargets(links), *cMap.IssueID)
	assert.NotContains(t, linkTargets(links), *bMap.IssueID)
	_, err = d.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
	_, err = d.ImportMappingBySource(ctx, p.ID, "beads", "link", "a:blocks:b")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_NonAuthoritativeParentLinksPreserveExistingSourceParentForChangedIssue(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Links: []db.ImportLink{{Type: "parent", TargetExternalID: "issue-id:102"}}},
		{ExternalID: "issue-id:102", Title: "Parent", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)
	parentMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:102")
	require.NoError(t, err)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, LinkTypesAuthoritative: map[string]bool{"parent": false}},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, 0, res.Links)
	for _, event := range events {
		assert.NotEqual(t, "issue.unlinked", event.Type)
	}

	parents, err := d.ParentNumbersByIssues(ctx, []int64{*childMap.IssueID})
	require.NoError(t, err)
	assert.Equal(t, *parentMap.IssueID, parents[*childMap.IssueID])
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:101:parent:issue-id:102")
	assert.NoError(t, err)
}

func TestImportBatch_AuthoritativeParentLinksRemoveMissingSourceParentForChangedIssue(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Links: []db.ImportLink{{Type: "parent", TargetExternalID: "issue-id:102"}}},
		{ExternalID: "issue-id:102", Title: "Parent", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)

	_, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, LinkTypesAuthoritative: map[string]bool{"parent": true}},
	}})
	require.NoError(t, err)

	var unlinked bool
	for _, event := range events {
		if event.Type == "issue.unlinked" {
			unlinked = true
		}
	}
	assert.True(t, unlinked)
	parents, err := d.ParentNumbersByIssues(ctx, []int64{*childMap.IssueID})
	require.NoError(t, err)
	assert.NotContains(t, parents, *childMap.IssueID)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:101:parent:issue-id:102")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_AuthoritativeParentDoesNotReplaceExistingLocalParent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "issue-id:102", Title: "Source parent", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "issue-id:103", Title: "Local parent", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)
	sourceParentMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:102")
	require.NoError(t, err)
	localParentMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:103")
	require.NoError(t, err)
	localLink := makeLink(ctx, t, d, *childMap.IssueID, *localParentMap.IssueID, "parent")

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID:                    p.ID,
		Source:                       "github:R_example",
		Actor:                        "github-sync",
		PreserveLocalParentConflicts: true,
		Items: []db.ImportItem{
			{ExternalID: "issue-id:101", Title: "Child changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, Links: []db.ImportLink{{Type: "parent", TargetExternalID: "issue-id:102"}}, LinkTypesAuthoritative: map[string]bool{"parent": true}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, 0, res.Links)
	for _, event := range events {
		assert.NotEqual(t, "issue.linked", event.Type)
	}

	parents, err := d.ParentNumbersByIssues(ctx, []int64{*childMap.IssueID})
	require.NoError(t, err)
	assert.Equal(t, *localParentMap.IssueID, parents[*childMap.IssueID])
	_, err = d.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
	_, err = d.LinkByEndpoints(ctx, *childMap.IssueID, *sourceParentMap.IssueID, "parent")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:101:parent:issue-id:102")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_ConflictingParentFailsUnlessLocalParentPreservationEnabled(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "child", Title: "Child", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "import-parent", Title: "Import parent", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "local-parent", Title: "Local parent", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "child")
	require.NoError(t, err)
	localParentMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "local-parent")
	require.NoError(t, err)
	makeLink(ctx, t, d, *childMap.IssueID, *localParentMap.IssueID, "parent")

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "child", Title: "Child changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, Links: []db.ImportLink{{Type: "parent", TargetExternalID: "import-parent"}}},
	}})
	require.ErrorIs(t, err, db.ErrParentAlreadySet)
}

func TestImportBatch_NonAuthoritativeParentDoesNotPreserveOtherLinkTypes(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Links: []db.ImportLink{
			{Type: "parent", TargetExternalID: "issue-id:102"},
			{Type: "blocks", TargetExternalID: "issue-id:103"},
		}},
		{ExternalID: "issue-id:102", Title: "Parent", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "issue-id:103", Title: "Blocked", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)
	parentMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:102")
	require.NoError(t, err)
	blockedMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:103")
	require.NoError(t, err)

	_, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:101", Title: "Child changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, LinkTypesAuthoritative: map[string]bool{"parent": false}},
	}})
	require.NoError(t, err)

	var unlinked bool
	for _, event := range events {
		if event.Type == "issue.unlinked" {
			unlinked = true
		}
	}
	assert.True(t, unlinked)
	parents, err := d.ParentNumbersByIssues(ctx, []int64{*childMap.IssueID})
	require.NoError(t, err)
	assert.Equal(t, *parentMap.IssueID, parents[*childMap.IssueID])
	_, err = d.LinkByEndpoints(ctx, *childMap.IssueID, *blockedMap.IssueID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:101:parent:issue-id:102")
	assert.NoError(t, err)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:101:blocks:issue-id:103")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_StaleSourceLinkMappingDoesNotUseIssueExternalIDAsLinkType(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:parent:101", Title: "Source ID with delimiter", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "issue-id:102"}}},
		{ExternalID: "issue-id:102", Title: "Blocked", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	childMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:parent:101")
	require.NoError(t, err)
	blockedMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "issue", "issue-id:102")
	require.NoError(t, err)
	linkMap, err := d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:parent:101:blocks:issue-id:102")
	require.NoError(t, err)
	require.NotNil(t, linkMap.LinkID)

	conn, err := d.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, *linkMap.LinkID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github:R_example", Actor: "github-sync", Items: []db.ImportItem{
		{ExternalID: "issue-id:parent:101", Title: "Source ID with delimiter changed", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, LinkTypesAuthoritative: map[string]bool{"parent": false}},
	}})
	require.NoError(t, err)

	_, err = d.LinkByEndpoints(ctx, *childMap.IssueID, *blockedMap.IssueID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github:R_example", "link", "issue-id:parent:101:blocks:issue-id:102")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_DoesNotAdoptPreExistingLocalLink(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Now().UTC().Add(-24 * time.Hour)
	t2 := time.Now().UTC().Add(time.Hour)
	t3 := time.Now().UTC().Add(2 * time.Hour)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	aMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	bMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "b")
	require.NoError(t, err)
	localLink := makeLink(ctx, t, d, *aMap.IssueID, *bMap.IssueID, "blocks")

	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A2", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "b"}}},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, 0, res.Links)
	_, err = d.ImportMappingBySource(ctx, p.ID, "beads", "link", "a:blocks:b")
	assert.ErrorIs(t, err, db.ErrNotFound)

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A3", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t3},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	_, err = d.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
	_, err = d.ImportMappingBySource(ctx, p.ID, "beads", "link", "a:blocks:b")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportBatch_ReimportCorrectsStoredCreatedAtAheadOfClosedAt(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	// First import stores a synthetic, late created_at (e.g. an older sync
	// that fell back to syncStartedAt when GitHub omitted created_at).
	synthetic := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	realCreated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	realClosed := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "open", Body: "body", Author: "alice", Status: "open", CreatedAt: synthetic, UpdatedAt: synthetic},
	}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue-id:1")
	require.NoError(t, err)
	stored, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	require.True(t, stored.CreatedAt.Equal(synthetic))

	// A later sync recovers the real (earlier) created_at and a real
	// closed_at that precedes the previously stored synthetic created_at.
	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "closed", Body: "body", Author: "alice", Status: "closed", ClosedReason: strPtr("done"), CreatedAt: realCreated, UpdatedAt: newer, ClosedAt: &realClosed},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)

	updated, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	require.NotNil(t, updated.ClosedAt)
	assert.True(t, updated.ClosedAt.Equal(realClosed))
	assert.True(t, updated.CreatedAt.Equal(realCreated),
		"created_at should move earlier to the corrected source timestamp, got %s", updated.CreatedAt)
	assert.False(t, updated.CreatedAt.After(*updated.ClosedAt),
		"persisted created_at %s must not be after closed_at %s", updated.CreatedAt, *updated.ClosedAt)
}

func TestImportBatch_ReimportDoesNotPushStoredCreatedAtLater(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	realCreated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	syntheticLater := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "old", Body: "body", Author: "alice", Status: "open", CreatedAt: realCreated, UpdatedAt: realCreated},
	}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue-id:1")
	require.NoError(t, err)

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "new", Body: "body", Author: "alice", Status: "open", CreatedAt: syntheticLater, UpdatedAt: newer},
	}})
	require.NoError(t, err)

	updated, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.True(t, updated.CreatedAt.Equal(realCreated),
		"created_at must not move later than the originally stored timestamp, got %s", updated.CreatedAt)
}

func TestImportBatch_ReimportSameVersionHealsInvertedCreatedAt(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	realCreated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	realClosed := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	synthetic := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	// First import stores a synthetic, late created_at.
	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "real-title", Body: "body", Author: "alice", Status: "open", CreatedAt: synthetic, UpdatedAt: newer},
	}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue-id:1")
	require.NoError(t, err)

	// Simulate the pre-patch damage: an older update path wrote the corrected
	// closed_at but left the synthetic late created_at, producing a row whose
	// created_at is after its closed_at.
	_, err = d.ExecContext(ctx, `UPDATE issues SET status='closed', closed_reason='done', closed_at=? WHERE id=?`, realClosed, *m.IssueID)
	require.NoError(t, err)
	damaged, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	require.NotNil(t, damaged.ClosedAt)
	require.True(t, damaged.CreatedAt.After(*damaged.ClosedAt), "precondition: row created_at is inverted past closed_at")

	// Reimport the SAME source version (updated_at unchanged) carrying the real,
	// earlier created_at. The source is not newer overall, so the regular update
	// path is skipped, but created_at must still heal earlier. Other fields from
	// the stale source item (title) must not overwrite the stored row.
	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "stale-title", Body: "body", Author: "alice", Status: "closed", ClosedReason: strPtr("done"), CreatedAt: realCreated, UpdatedAt: newer, ClosedAt: &realClosed},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)

	healed, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	assert.True(t, healed.CreatedAt.Equal(realCreated),
		"created_at should heal to corrected source timestamp, got %s", healed.CreatedAt)
	require.NotNil(t, healed.ClosedAt)
	assert.False(t, healed.CreatedAt.After(*healed.ClosedAt),
		"persisted created_at %s must not be after closed_at %s", healed.CreatedAt, *healed.ClosedAt)
	assert.Equal(t, "real-title", healed.Title, "heal must not overwrite other fields from a stale source item")

	var repair *db.Event
	for i := range events {
		if events[i].Type == "issue.updated" {
			repair = &events[i]
		}
	}
	require.NotNil(t, repair, "heal must emit an issue.updated event")
	assertEventCarriesCreatedAt(t, *repair, healed)
}

func TestImportBatch_ReimportSameVersionDoesNotEmitWhenCreatedAtUnchanged(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "t", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)

	res, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "t", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Unchanged)
	assert.Empty(t, events, "unchanged reimport must not emit events")
}

func TestImportBatch_NewerReimportCarriesCreatedAtInPayload(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	synthetic := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	realCreated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	realClosed := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "open", Body: "body", Author: "alice", Status: "open", CreatedAt: synthetic, UpdatedAt: synthetic},
	}})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue-id:1")
	require.NoError(t, err)

	_, events, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", Title: "closed", Body: "body", Author: "alice", Status: "closed", ClosedReason: strPtr("done"), CreatedAt: realCreated, UpdatedAt: newer, ClosedAt: &realClosed},
	}})
	require.NoError(t, err)
	updated, err := d.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)

	var repair *db.Event
	for i := range events {
		if events[i].Type == "issue.updated" {
			repair = &events[i]
		}
	}
	require.NotNil(t, repair, "newer reimport must emit an issue.updated event")
	assertEventCarriesCreatedAt(t, *repair, updated)
}

func TestImportBatch_ReimportRecreatesLinkWhenMappingReferencesStaleLink(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "b"}}},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "c", Title: "C", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	aMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	require.NoError(t, err)
	bMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "b")
	require.NoError(t, err)
	cMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "c")
	require.NoError(t, err)
	linkMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "link", "a:blocks:b")
	require.NoError(t, err)
	require.NotNil(t, linkMap.LinkID)
	oldLinkID := *linkMap.LinkID

	localLink := makeLink(ctx, t, d, *aMap.IssueID, *cMap.IssueID, "related")
	_, err = d.ExecContext(ctx, `UPDATE import_mappings SET link_id = ? WHERE id = ?`, localLink.ID, linkMap.ID)
	require.NoError(t, err)
	require.NoError(t, d.DeleteLinkByID(ctx, oldLinkID))

	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "a", Title: "A2", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t2, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "b"}}},
		{ExternalID: "b", Title: "B", Body: "body", Author: "bob", Status: "open", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "c", Title: "C", Body: "body", Author: "cara", Status: "open", CreatedAt: t1, UpdatedAt: t1},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, 1, res.Links)

	recreated, err := d.LinkByEndpoints(ctx, *aMap.IssueID, *bMap.IssueID, "blocks")
	require.NoError(t, err)
	assert.NotEqual(t, oldLinkID, recreated.ID)
	assert.NotEqual(t, localLink.ID, recreated.ID)
	updatedMap, err := d.ImportMappingBySource(ctx, p.ID, "beads", "link", "a:blocks:b")
	require.NoError(t, err)
	require.NotNil(t, updatedMap.LinkID)
	assert.Equal(t, recreated.ID, *updatedMap.LinkID)
	_, err = d.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
}

func TestImportBatch_MissingLinkTargetRejectsTransaction(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Body: "body", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "missing"}}}}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound))
	_, err = d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "a")
	assert.ErrorIs(t, err, db.ErrNotFound)
	assertRowCount(ctx, t, d, 0, "issue insert rolled back", `SELECT COUNT(*) FROM issues WHERE project_id = ?`, p.ID)
}

func TestImportBatch_AdoptsLegacyExternalIDMapping(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	// A pre-upgrade sync keyed issues and comments by node_id.
	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue:I_node", Title: "Issue", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1,
			Comments: []db.ImportComment{{ExternalID: "comment:C_node", Author: "alice", Body: "note", CreatedAt: t1}}},
	}})
	require.NoError(t, err)
	legacyIssue, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue:I_node")
	require.NoError(t, err)
	require.NotNil(t, legacyIssue.IssueID)
	originalIssueID := *legacyIssue.IssueID
	legacyComment, err := d.ImportMappingBySource(ctx, p.ID, "github", "comment", "comment:C_node")
	require.NoError(t, err)
	require.NotNil(t, legacyComment.CommentID)
	originalCommentID := *legacyComment.CommentID

	// After upgrade the REST id is canonical; the node_id key is supplied as a
	// legacy alias so the existing rows are adopted instead of duplicated.
	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "github", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "issue-id:1", LegacyExternalIDs: []string{"issue:I_node"}, Title: "Issue", Body: "body", Author: "alice", Status: "open", CreatedAt: t1, UpdatedAt: t1,
			Comments: []db.ImportComment{{ExternalID: "comment-id:2", LegacyExternalIDs: []string{"comment:C_node"}, Author: "alice", Body: "note", CreatedAt: t1}}},
	}})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Created, "legacy mapping must be adopted, not recreated")
	assert.Equal(t, 1, res.Unchanged)
	assert.Equal(t, 0, res.Comments, "legacy comment mapping must be adopted, not recreated")

	assertRowCount(ctx, t, d, 1, "no duplicate issue", `SELECT COUNT(*) FROM issues WHERE project_id = ?`, p.ID)
	assertRowCount(ctx, t, d, 1, "no duplicate comment", `SELECT COUNT(*) FROM comments WHERE issue_id = ?`, originalIssueID)

	canonicalIssue, err := d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue-id:1")
	require.NoError(t, err)
	require.NotNil(t, canonicalIssue.IssueID)
	assert.Equal(t, originalIssueID, *canonicalIssue.IssueID)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github", "issue", "issue:I_node")
	assert.ErrorIs(t, err, db.ErrNotFound, "legacy issue mapping should be re-keyed, not left behind")

	canonicalComment, err := d.ImportMappingBySource(ctx, p.ID, "github", "comment", "comment-id:2")
	require.NoError(t, err)
	require.NotNil(t, canonicalComment.CommentID)
	assert.Equal(t, originalCommentID, *canonicalComment.CommentID)
	_, err = d.ImportMappingBySource(ctx, p.ID, "github", "comment", "comment:C_node")
	assert.ErrorIs(t, err, db.ErrNotFound, "legacy comment mapping should be re-keyed, not left behind")
}

func TestImportBatch_ValidationErrors(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "", Actor: "importer"})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "bad", CreatedAt: ts, UpdatedAt: ts}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "closed", ClosedReason: strPtr(""), CreatedAt: ts, UpdatedAt: ts, ClosedAt: &ts}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "closed", ClosedReason: strPtr("obsolete"), CreatedAt: ts, UpdatedAt: ts, ClosedAt: &ts}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "closed", ClosedReason: strPtr(" done "), CreatedAt: ts, UpdatedAt: ts, ClosedAt: &ts}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts, Links: []db.ImportLink{{Type: "bad", TargetExternalID: "b"}}}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts, Labels: []string{"UPPER"}}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: ts, UpdatedAt: ts, Labels: []string{strings.Repeat("a", 65)}}}})
	assert.ErrorIs(t, err, db.ErrImportValidation)
}

func TestImportBatch_AcceptsAllSchemaClosedReasons(t *testing.T) {
	// Schema v8 accepts done, wontfix, duplicate, superseded, and
	// audit-no-change. The import validator must agree with the schema
	// or callers cannot replay closed issues that used the newer reasons.
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for _, reason := range []string{"done", "wontfix", "duplicate", "superseded", "audit-no-change"} {
		t.Run(reason, func(t *testing.T) {
			d, ctx, p := setupTestProject(t)
			_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{
				ProjectID: p.ID,
				Source:    "beads",
				Actor:     "importer",
				Items: []db.ImportItem{{
					ExternalID:   "a",
					Title:        "Title",
					Author:       "alice",
					Status:       "closed",
					ClosedReason: strPtr(reason),
					CreatedAt:    ts,
					UpdatedAt:    ts,
					ClosedAt:     &ts,
				}},
			})
			require.NoError(t, err)
		})
	}
}

func TestImportBatch_TimestampValidationErrors(t *testing.T) {
	t.Run("updated before created", func(t *testing.T) {
		d, ctx, p := setupTestProject(t)
		createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		updatedAt := createdAt.Add(-time.Second)
		_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: createdAt, UpdatedAt: updatedAt}}})
		assert.ErrorIs(t, err, db.ErrImportValidation)
	})
	t.Run("closed before created", func(t *testing.T) {
		d, ctx, p := setupTestProject(t)
		createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		closedAt := createdAt.Add(-time.Second)
		_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "closed", CreatedAt: createdAt, UpdatedAt: createdAt, ClosedAt: &closedAt}}})
		assert.ErrorIs(t, err, db.ErrImportValidation)
	})
	t.Run("closed at on open", func(t *testing.T) {
		d, ctx, p := setupTestProject(t)
		createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "open", CreatedAt: createdAt, UpdatedAt: createdAt, ClosedAt: &createdAt}}})
		assert.ErrorIs(t, err, db.ErrImportValidation)
	})
	t.Run("closed missing closed at", func(t *testing.T) {
		d, ctx, p := setupTestProject(t)
		createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "beads", Actor: "importer", Items: []db.ImportItem{{ExternalID: "a", Title: "A", Author: "alice", Status: "closed", CreatedAt: createdAt, UpdatedAt: createdAt}}})
		assert.ErrorIs(t, err, db.ErrImportValidation)
	})
}

func commentsForIssue(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64) []db.Comment {
	t.Helper()
	rows, err := d.QueryContext(ctx, `SELECT id, uid, issue_id, author, body, created_at FROM comments WHERE issue_id = ? ORDER BY id ASC`, issueID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []db.Comment
	for rows.Next() {
		var c db.Comment
		require.NoError(t, rows.Scan(&c.ID, &c.UID, &c.IssueID, &c.Author, &c.Body, &c.CreatedAt))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func labelsForIssue(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64) []db.IssueLabel {
	t.Helper()
	rows, err := d.QueryContext(ctx, `SELECT issue_id, label, author, created_at FROM issue_labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []db.IssueLabel
	for rows.Next() {
		var l db.IssueLabel
		require.NoError(t, rows.Scan(&l.IssueID, &l.Label, &l.Author, &l.CreatedAt))
		out = append(out, l)
	}
	require.NoError(t, rows.Err())
	return out
}

func linksForIssue(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64) []db.Link {
	t.Helper()
	rows, err := d.QueryContext(ctx, `SELECT id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at FROM links WHERE from_issue_id = ? OR to_issue_id = ? ORDER BY id ASC`, issueID, issueID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []db.Link
	for rows.Next() {
		var l db.Link
		require.NoError(t, rows.Scan(&l.ID, &l.FromIssueID, &l.FromIssueUID, &l.ToIssueID, &l.ToIssueUID, &l.Type, &l.Author, &l.CreatedAt))
		out = append(out, l)
	}
	require.NoError(t, rows.Err())
	return out
}

func labelNames(labels []db.IssueLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Label)
	}
	return out
}

func linkTargets(links []db.Link) []int64 {
	out := make([]int64, 0, len(links))
	for _, l := range links {
		out = append(out, l.ToIssueID)
	}
	return out
}
