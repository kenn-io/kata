package dbtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

// checkArchivedLinkTargets pins the add-side link contract inside the mutation
// transaction: an initial link, an atomic-edit add or set-parent, and a
// standalone link insert must all reject a target whose project is archived
// at commit time, while removals of existing links to archived targets keep
// working. Daemon handlers gate the same condition before the transaction;
// the store check catches archival that races that preflight.
func checkArchivedLinkTargets(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	subject, err := createIssueFixture(ctx, store, "archived-link-subject", "subject", "link-author", nil)
	if err != nil {
		return err
	}
	peer, err := createIssueFixture(ctx, store, "archived-link-peer", "future parent", "link-author", nil)
	if err != nil {
		return err
	}
	linked, err := createFixtureIssue(ctx, store, peer.Project.ID, "already related", "link-author", nil)
	if err != nil {
		return err
	}
	if _, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: subject.Issue.ID, ToIssueID: linked.ID, Type: "related", Author: "link-author",
	}); err != nil {
		return fmt.Errorf("create pre-archive related link: %w", err)
	}
	if _, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: peer.Project.ID, Actor: "link-author", Force: true}); err != nil {
		return fmt.Errorf("archive peer project: %w", err)
	}

	var archived *db.LinkTargetArchivedError
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: subject.Project.ID, Title: "links into archived project", Author: "link-author",
		Links: []db.InitialLink{{Type: "related", ToNumber: peer.Issue.ID}},
	})
	if assert.ErrorAs(t, err, &archived, "initial link to archived target") && archived != nil {
		assert.Equal(t, peer.Issue.ID, archived.Number)
		assert.Equal(t, peer.Project.Name, archived.Project)
	}

	title := "edit must roll back"
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: subject.Issue.ID, Actor: "link-author", Title: &title, AddBlocks: []int64{peer.Issue.ID},
	})
	assert.ErrorAs(t, err, &archived, "atomic edit add to archived target")
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: subject.Issue.ID, Actor: "link-author", SetParent: &peer.Issue.ID,
	})
	assert.ErrorAs(t, err, &archived, "atomic edit set_parent to archived target")
	afterRollback, err := store.IssueByID(ctx, subject.Issue.ID)
	if err != nil {
		return fmt.Errorf("load subject after rejected edits: %w", err)
	}
	assert.Equal(t, subject.Issue.Title, afterRollback.Title, "rejected edit must roll back field changes")

	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: subject.Issue.ID, ToIssueID: peer.Issue.ID, Type: "blocks", Author: "link-author",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: subject.Issue.ID,
		FromShortID: subject.Issue.ShortID, FromUID: subject.Issue.UID,
		ToShortID: peer.Issue.ShortID, ToUID: peer.Issue.UID, Actor: "link-author",
	})
	assert.ErrorAs(t, err, &archived, "standalone link insert to archived target")

	result, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: subject.Issue.ID, Actor: "link-author", RemoveRelated: []int64{linked.ID},
	})
	if err != nil {
		return fmt.Errorf("remove existing link to archived target: %w", err)
	}
	assert.True(t, result.AnyChange, "existing links to archived targets must stay removable")
	links, err := store.LinksByIssue(ctx, subject.Issue.ID)
	if err != nil {
		return fmt.Errorf("links after removal: %w", err)
	}
	assert.Empty(t, links)
	return nil
}
