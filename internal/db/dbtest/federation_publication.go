package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// PublicationFixture exercises the portable contract with deliberately sparse
// row IDs, longer stored short IDs, historical names and comment reply/edit data.
func PublicationFixture(t *testing.T, targetUID string) db.FederationPublication {
	t.Helper()
	const stamp = "2026-10-20T12:00:00.000Z"
	const project = "01K00000000000000000000001"
	const first = "01K00000000000000000000002"
	const second = "01K00000000000000000000003"
	const source = "01K00000000000000000000004"
	const comment = "01K00000000000000000000006"
	const reply = "01K00000000000000000000007"
	records := []db.ImportRecord{
		&db.ProjectExport{ID: 71, UID: project, Name: "published-project", CreatedAt: stamp, Metadata: jsontext.Value(`{}`), Revision: 1},
		&db.IssueExport{ID: 81, UID: first, ProjectID: 71, ShortID: "000002", Title: "First", Author: "original-author", Status: "open", CreatedAt: stamp, UpdatedAt: stamp, Metadata: jsontext.Value(`{}`), Revision: 1, ContentRevision: 1},
		&db.IssueExport{ID: 82, UID: second, ProjectID: 71, ShortID: "000003", Title: "Second", Author: "other-author", Status: "open", CreatedAt: stamp, UpdatedAt: stamp, Metadata: jsontext.Value(`{}`), Revision: 1, ContentRevision: 1},
		&db.CommentExport{ID: 91, UID: comment, IssueID: 81, Author: "comment-author", Teammate: "reviewer", Body: "Original", CreatedAt: stamp},
		&db.CommentExport{ID: 92, UID: reply, IssueID: 81, Author: "reply-author", Body: "Edited reply", CreatedAt: stamp, ReplyToUID: comment, ReplyKind: "reply", EditedAt: stamp},
		&db.IssueLabelExport{IssueID: 81, Label: "portable", Author: "original-author", CreatedAt: stamp},
		&db.LinkExport{ID: 101, FromIssueID: 81, FromIssueUID: first, ToIssueID: 82, ToIssueUID: second, Type: "blocks", Author: "original-author", CreatedAt: stamp},
	}
	for n, entry := range []struct {
		id                               int64
		uid, issue, title, author, extra string
	}{
		{82, "01K00000000000000000000008", second, "Second", "other-author", `"short_id":"000003"`},
		{81, "01K00000000000000000000009", first, "First", "original-author", `"short_id":"000002","labels":["portable"],"links":[{"type":"blocks","to_issue_uid":"` + second + `","author":"original-author","created_at":"` + stamp + `"}],"comments":[{"comment_uid":"` + comment + `","author":"comment-author","teammate":"reviewer","body":"Original","created_at":"` + stamp + `"},{"comment_uid":"` + reply + `","author":"reply-author","body":"Edited reply","created_at":"` + stamp + `","reply_to_uid":"` + comment + `","reply_kind":"reply","edited_at":"` + stamp + `"}]`},
	} {
		payload := `{"uid":"` + entry.issue + `","title":"` + entry.title + `","author":"` + entry.author + `","body":"","status":"open","created_at":"` + stamp + `","updated_at":"` + stamp + `","metadata":{},` + entry.extra + `}`
		e := &db.EventExport{ID: int64(110 + n), UID: entry.uid, OriginInstanceUID: source, ProjectID: 71, ProjectUID: project, ProjectName: "former-project", IssueID: new(entry.id), IssueUID: new(entry.issue), Type: "issue.snapshot", Actor: entry.author, Payload: jsontext.Value(payload), HLCPhysicalMS: 4102444800000, HLCCounter: int64(n), CreatedAt: stamp}
		if n == 1 {
			e.RelatedIssueID = new(int64(82))
			e.RelatedIssueUID = new(second)
		}
		require.NoError(t, db.PrepareReplayEvent(e, project, e.ProjectName, true))
		records = append(records, e)
	}
	p := db.FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: source, ProjectUID: project, TargetInstanceUID: targetUID, ProjectName: "published-project", Records: records}
	_, err := db.ValidateFederationPublication(p)
	require.NoError(t, err)
	p.Digest = strings.Repeat("a", 64)
	return p
}

// RunFederationPublication is registered by the integration owner in the shared
// conformance table. Native backend tests can also call it directly.
func RunFederationPublication(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	publisher, ok := store.(db.FederationPublicationStore)
	require.True(t, ok, "native store must implement account-authorized online publication")
	ctx := t.Context()
	p := PublicationFixture(t, store.InstanceUID())
	before, err := json.Marshal(p.Records)
	require.NoError(t, err)
	result, err := publisher.PublishFederationProject(ctx, p, "publisher")
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.NotNil(t, result.CatalogEvent)
	require.Equal(t, "publisher", result.CatalogEvent.Actor)
	require.Equal(t, "project.created", result.CatalogEvent.Type)
	require.Equal(t, p.ProjectUID, result.Project.UID)
	require.NotEqual(t, int64(71), result.Project.ID)
	for _, record := range p.Records {
		switch r := record.(type) {
		case *db.IssueExport:
			issue, err := store.IssueByUID(ctx, r.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, r.ShortID, issue.ShortID)
			require.Equal(t, r.Author, issue.Author)
			require.NotEqual(t, r.ID, issue.ID)
			require.Equal(t, result.Project.ID, issue.ProjectID)
		case *db.CommentExport:
			issue, err := store.IssueByUID(ctx, "01K00000000000000000000002", db.IncludeDeletedYes)
			require.NoError(t, err)
			comments, err := store.CommentsByIssue(ctx, issue.ID)
			require.NoError(t, err)
			var found bool
			for _, c := range comments {
				if c.UID == r.UID {
					found = true
					require.Equal(t, r.Author, c.Author)
					require.Equal(t, r.Teammate, c.Teammate)
					require.Equal(t, r.Body, c.Body)
					require.Equal(t, r.ReplyToUID, c.ReplyToUID)
					require.Equal(t, r.ReplyKind, c.ReplyKind)
					if r.EditedAt != "" {
						require.NotNil(t, c.EditedAt)
						require.Equal(t, r.EditedAt, c.EditedAt.UTC().Format("2006-01-02T15:04:05.000Z"))
					}
				}
			}
			require.True(t, found)
		case *db.EventExport:
			events, err := store.EventsByUIDs(ctx, result.Project.ID, []string{r.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			e := events[0]
			require.Equal(t, r.OriginInstanceUID, e.OriginInstanceUID)
			require.Equal(t, r.ProjectName, e.ProjectName)
			require.Equal(t, string(r.Payload), e.Payload)
			require.Equal(t, r.ContentHash, e.ContentHash)
			require.Equal(t, r.Actor, e.Actor)
			require.Equal(t, r.HLCPhysicalMS, e.HLCPhysicalMS)
			require.Equal(t, r.HLCCounter, e.HLCCounter)
			require.Greater(t, result.CatalogEvent.ID, e.ID)
		}
	}
	after, err := json.Marshal(p.Records)
	require.NoError(t, err)
	require.Equal(t, before, after, "publication never mutates caller records")
	last := p.Records[len(p.Records)-1].(*db.EventExport)
	require.True(t, result.CatalogEvent.HLCPhysicalMS > last.HLCPhysicalMS || (result.CatalogEvent.HLCPhysicalMS == last.HLCPhysicalMS && result.CatalogEvent.HLCCounter > last.HLCCounter), "new native clock must follow imported future history")
	// Editing an originally imported comment must survive retry too.
	importedIssue, err := store.IssueByUID(ctx, "01K00000000000000000000002", db.IncludeDeletedYes)
	require.NoError(t, err)
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: importedIssue.ID, CommentUID: "01K00000000000000000000007", Actor: "reply-author", Body: "Changed on hub"})
	require.NoError(t, err)
	oldID := result.Project.ID
	renamed, err := store.RenameProject(ctx, oldID, "renamed-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: oldID, Title: "Hub edit", Author: "hub-author"})
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	retry, err := publisher.PublishFederationProject(ctx, p, "publisher")
	require.NoError(t, err)
	require.True(t, retry.Replayed)
	require.Nil(t, retry.CatalogEvent)
	require.Equal(t, renamed, retry.Project)
	require.Equal(t, oldID, retry.Project.ID)
	comments, err := store.CommentsByIssue(ctx, importedIssue.ID)
	require.NoError(t, err)
	for _, comment := range comments {
		if comment.UID == "01K00000000000000000000007" {
			require.Equal(t, "Changed on hub", comment.Body)
		}
	}

	current, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, current)
	for _, change := range []string{"digest", "account", "target", "source", "operation", "project"} {
		t.Run("mismatch_"+change, func(t *testing.T) {
			other := p
			actor := "publisher"
			switch change {
			case "digest":
				other.Digest = strings.Repeat("f", 64)
			case "account":
				actor = "other-account"
			case "target":
				other.TargetInstanceUID = "01K00000000000000000000010"
			case "source":
				other.SourceInstanceUID = "01K00000000000000000000010"
			case "operation":
				other.OperationUID = "01K00000000000000000000010"
			case "project":
				other.ProjectUID = "01K00000000000000000000010"
			}
			_, err := publisher.PublishFederationProject(ctx, other, actor)
			require.Error(t, err)
			if change != "project" {
				require.ErrorIs(t, err, db.ErrFederationPublicationConflict)
			}
		})
	}
	// Current fences also guard committed retries; a marker grants no authority.
	denied := errors.New("current authorization denied")
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return denied })
	_, err = publisher.PublishFederationProject(fenced, p, "publisher")
	require.ErrorIs(t, err, denied)
	for _, collision := range []string{"empty_name", "active_name", "archived_name", "uid"} {
		t.Run(collision, func(t *testing.T) {
			target := backend.Open(t)
			t.Cleanup(func() { _ = target.Close() })
			fixture := PublicationFixture(t, target.InstanceUID())
			name, projectUID := fixture.ProjectName, "01K00000000000000000000011"
			if collision == "uid" {
				name = "other-project"
				projectUID = fixture.ProjectUID
			}
			existing, err := target.CreateProjectWithUID(ctx, name, projectUID)
			require.NoError(t, err)
			if collision == "active_name" {
				_, _, err = target.CreateIssue(ctx, db.CreateIssueParams{ProjectID: existing.ID, Title: "Existing", Author: "author"})
				require.NoError(t, err)
			}
			if collision == "archived_name" {
				_, _, err = target.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: existing.ID, Actor: "author"})
				require.NoError(t, err)
			}
			_, err = target.(db.FederationPublicationStore).PublishFederationProject(ctx, fixture, "publisher")
			require.ErrorIs(t, err, db.ErrFederationPublicationConflict)
		})
	}
}

// RunFederationPublicationRelatedEndpointOrder verifies that a reversed source
// record order keeps both UID and numeric endpoints canonical on import.
func RunFederationPublicationRelatedEndpointOrder(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	p := PublicationFixture(t, store.InstanceUID())
	firstIndex, secondIndex := -1, -1
	var firstUID, secondUID string
	for index, record := range p.Records {
		if issue, ok := record.(*db.IssueExport); ok {
			if issue.ID == 81 {
				firstIndex, firstUID = index, issue.UID
			}
			if issue.ID == 82 {
				secondIndex, secondUID = index, issue.UID
			}
		}
		if link, ok := record.(*db.LinkExport); ok {
			link.Type = "related"
		}
	}
	require.NotEqual(t, -1, firstIndex)
	require.NotEqual(t, -1, secondIndex)
	p.Records[firstIndex], p.Records[secondIndex] = p.Records[secondIndex], p.Records[firstIndex]
	for _, record := range p.Records {
		event, ok := record.(*db.EventExport)
		if !ok || event.IssueUID == nil || *event.IssueUID != firstUID {
			continue
		}
		var payload map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		if payload == nil {
			t.Fatal("issue snapshot payload must be an object")
		}
		var links []map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(payload["links"], &links))
		for _, embedded := range links {
			if string(embedded["type"]) == `"blocks"` {
				embedded["type"] = jsontext.Value(`"related"`)
			}
		}
		rawLinks, err := json.Marshal(links)
		require.NoError(t, err)
		payload["links"] = rawLinks
		rawPayload, err := json.Marshal(payload)
		require.NoError(t, err)
		event.Payload = rawPayload
		require.NoError(t, db.PrepareReplayEvent(event, p.ProjectUID, event.ProjectName, true))
	}

	publisher, ok := store.(db.FederationPublicationStore)
	require.True(t, ok)
	result, err := publisher.PublishFederationProject(ctx, p, "publisher")
	require.NoError(t, err)
	first, err := store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	second, err := store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Less(t, second.ID, first.ID, "reversed records allocate the second source issue first")
	links, err := store.LinksByIssue(ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Less(t, links[0].FromIssueID, links[0].ToIssueID)
	from, err := store.IssueByID(ctx, links[0].FromIssueID)
	require.NoError(t, err)
	to, err := store.IssueByID(ctx, links[0].ToIssueID)
	require.NoError(t, err)
	require.Equal(t, from.UID, links[0].FromIssueUID)
	require.Equal(t, to.UID, links[0].ToIssueUID)
	require.ElementsMatch(t, []string{firstUID, secondUID}, []string{links[0].FromIssueUID, links[0].ToIssueUID})
	require.Equal(t, p.ProjectUID, result.Project.UID)
}

// RunFederationPublicationRollback proves failed native inserts leave no publication state.
func RunFederationPublicationRollback(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	publisher, ok := store.(db.FederationPublicationStore)
	require.True(t, ok)
	ctx := t.Context()
	p := PublicationFixture(t, store.InstanceUID())
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var install, drop string
	if backend.Name == "postgres" {
		_, err := q.ExecContext(ctx, `CREATE FUNCTION publication_fail_comment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected publication insertion failure'; END $$`)
		require.NoError(t, err)
		install = `CREATE TRIGGER publication_fail_comment BEFORE INSERT ON comments FOR EACH ROW EXECUTE FUNCTION publication_fail_comment()`
		drop = `DROP TRIGGER publication_fail_comment ON comments`
	} else {
		install = `CREATE TRIGGER publication_fail_comment BEFORE INSERT ON comments BEGIN SELECT RAISE(FAIL, 'injected publication insertion failure'); END`
		drop = `DROP TRIGGER publication_fail_comment`
	}
	_, err := q.ExecContext(ctx, install)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = publisher.PublishFederationProject(ctx, p, "publisher")
	require.Error(t, err)
	var count int
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE uid=$1`, p.ProjectUID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE uid=$1`, "01K00000000000000000000002").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, "federation.publication.v1/"+p.OperationUID).Scan(&count))
	require.Zero(t, count)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after)
	_, err = q.ExecContext(ctx, drop)
	require.NoError(t, err)
	result, err := publisher.PublishFederationProject(ctx, p, "publisher")
	require.NoError(t, err)
	require.False(t, result.Replayed)
}

// RunFederationPublicationCapacity enforces the destination checkpoint budget atomically.
func RunFederationPublicationCapacity(t *testing.T, _ db.Storage, backend Backend) {
	t.Helper()
	if backend.OpenWithRelayCheckpointCapacity == nil {
		t.Fatal("native capacity fixture required")
	}
	store := backend.OpenWithRelayCheckpointCapacity(t, 1024)
	p := PublicationFixture(t, store.InstanceUID())
	publisher, ok := store.(db.FederationPublicationStore)
	require.True(t, ok)
	_, err := publisher.PublishFederationProject(t.Context(), p, "publisher")
	require.ErrorIs(t, err, db.ErrFederationPublicationTooLarge)
	_, err = store.ProjectByUID(t.Context(), p.ProjectUID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

// RunFederationPublicationCapacityIncludesFederationEnable refuses a project
// whose initial federation baseline would make its checkpoint too large.
func RunFederationPublicationCapacityIncludesFederationEnable(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	require.NotNil(t, backend.OpenWithRelayCheckpointCapacity, "native capacity fixture required")
	publisher, ok := store.(db.FederationPublicationStore)
	require.True(t, ok)
	measurer, ok := store.(db.RelayCheckpointMeasurer)
	require.True(t, ok)

	probe := PublicationFixture(t, store.InstanceUID())
	probeResult, err := publisher.PublishFederationProject(t.Context(), probe, "publisher")
	require.NoError(t, err)
	before, err := measurer.RelayCheckpointSize(t.Context(), probeResult.Project.ID)
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), probeResult.Project.ID, "publisher")
	require.NoError(t, err)
	after, err := measurer.RelayCheckpointSize(t.Context(), probeResult.Project.ID)
	require.NoError(t, err)
	require.Greater(t, after.Bytes, before.Bytes, "the federation baseline adds checkpoint history")

	delta := after.Bytes - before.Bytes
	capacity := before.Bytes + delta*3/4
	require.Greater(t, capacity, before.Bytes, "the project fits before enablement")
	require.Greater(t, after.Bytes, capacity, "the project exceeds capacity after enablement")

	limited := backend.OpenWithRelayCheckpointCapacity(t, capacity)
	limitedPublisher, ok := limited.(db.FederationPublicationStore)
	require.True(t, ok)
	_, err = limitedPublisher.PublishFederationProject(t.Context(), PublicationFixture(t, limited.InstanceUID()), "publisher")
	require.ErrorIs(t, err, db.ErrFederationPublicationTooLarge)
	_, err = limited.ProjectByUID(t.Context(), probe.ProjectUID)
	require.ErrorIs(t, err, db.ErrNotFound, "an over-capacity post-enable project is not published")
}
