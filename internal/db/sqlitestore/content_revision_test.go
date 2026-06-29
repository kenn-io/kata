package sqlitestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// contentRev reads issues.content_revision for issueID directly.
func contentRev(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64) int64 {
	t.Helper()
	var cr int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT content_revision FROM issues WHERE id = ?`, issueID).Scan(&cr))
	return cr
}

func TestContentRevisionBumpsOnTitleBodyOnly(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	proj := createProject(ctx, t, d, "spoke-project")
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "first", Body: "b", Author: "tester",
	})
	require.NoError(t, err)
	base := contentRev(ctx, t, d, iss.ID)

	// Title edit bumps.
	newTitle := "second"
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: iss.ID, Title: &newTitle, Actor: "tester"})
	require.NoError(t, err)
	afterTitle := contentRev(ctx, t, d, iss.ID)
	require.Equalf(t, base+1, afterTitle, "title edit must bump content_revision")

	// Body edit bumps.
	newBody := "c"
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: iss.ID, Body: &newBody, Actor: "tester"})
	require.NoError(t, err)
	afterBody := contentRev(ctx, t, d, iss.ID)
	require.Equalf(t, afterTitle+1, afterBody, "body edit must bump content_revision")

	// Re-applying the same title does NOT bump (no-op edit).
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: iss.ID, Title: &newTitle, Actor: "tester"})
	require.NoError(t, err)
	require.Equalf(t, afterBody, contentRev(ctx, t, d, iss.ID),
		"re-applying the same title must not bump content_revision")

	// Owner edit does NOT bump.
	owner := "alice"
	_, _, _, err = d.EditIssue(ctx, db.EditIssueParams{IssueID: iss.ID, Owner: &owner, Actor: "tester"})
	require.NoError(t, err)
	require.Equalf(t, afterBody, contentRev(ctx, t, d, iss.ID),
		"owner edit must not bump content_revision")

	// Comment does NOT bump.
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: iss.ID, Body: "c", Author: "tester"})
	require.NoError(t, err)
	require.Equalf(t, afterBody, contentRev(ctx, t, d, iss.ID),
		"comment must not bump content_revision")
}

func TestContentRevisionBumpsFromEditIssueAtomic(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	proj := createProject(ctx, t, d, "spoke-project")
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "first", Body: "b", Author: "tester",
	})
	require.NoError(t, err)
	base := contentRev(ctx, t, d, iss.ID)

	// Atomic title edit bumps.
	newTitle := "second"
	_, err = d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: iss.ID, Title: &newTitle, Actor: "tester"})
	require.NoError(t, err)
	afterTitle := contentRev(ctx, t, d, iss.ID)
	require.Equalf(t, base+1, afterTitle, "atomic title edit must bump content_revision")

	// Atomic priority-only edit does NOT bump.
	prio := int64(2)
	_, err = d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: iss.ID, SetPriority: &prio, Actor: "tester"})
	require.NoError(t, err)
	require.Equalf(t, afterTitle, contentRev(ctx, t, d, iss.ID),
		"atomic priority edit must not bump content_revision")
}

func TestContentRevisionBumpsFromImport(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	proj := createProject(ctx, t, d, "spoke-project")

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)

	// Initial import creates the issue.
	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: proj.ID, Source: "beads", Actor: "importer",
		Items: []db.ImportItem{{
			ExternalID: "a", Title: "first", Body: "b", Author: "alice",
			Status: "open", CreatedAt: t1, UpdatedAt: t1,
		}},
	})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, proj.ID, "beads", "issue", "a")
	require.NoError(t, err)
	require.NotNil(t, m.IssueID)
	issueID := *m.IssueID
	base := contentRev(ctx, t, d, issueID)

	// Re-import the same ExternalID with a changed Title (newer UpdatedAt so
	// updateImportedIssue runs) bumps content_revision.
	res, _, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: proj.ID, Source: "beads", Actor: "importer",
		Items: []db.ImportItem{{
			ExternalID: "a", Title: "second", Body: "b", Author: "alice",
			Status: "open", CreatedAt: t1, UpdatedAt: t2,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Updated)
	afterTitle := contentRev(ctx, t, d, issueID)
	require.Equalf(t, base+1, afterTitle, "import title change must bump content_revision")

	// Re-import again with same Title/Body but a non-content change (status,
	// owner) and newer UpdatedAt. updateImportedIssue runs but must NOT bump.
	res, _, err = d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: proj.ID, Source: "beads", Actor: "importer",
		Items: []db.ImportItem{{
			ExternalID: "a", Title: "second", Body: "b", Author: "alice",
			Owner: strPtr("bob"), Status: "closed", ClosedReason: strPtr("done"),
			CreatedAt: t1, UpdatedAt: t3, ClosedAt: &t3,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Updated)
	require.Equalf(t, afterTitle, contentRev(ctx, t, d, issueID),
		"import non-content change must not bump content_revision")
}

func TestContentRevisionBumpsFromImportPresentationTitleCorrection(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	proj := createProject(ctx, t, d, "spoke-project")
	sourceTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: proj.ID, Source: "github:R_example", Actor: "sync-agent",
		Items: []db.ImportItem{{
			ExternalID: "issue-id:101", Title: "Original title", Body: "body",
			Author: "sync-agent", Status: "open",
			CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime,
		}},
	})
	require.NoError(t, err)
	m, err := d.ImportMappingBySource(ctx, proj.ID, "github:R_example", "issue", "issue-id:101")
	require.NoError(t, err)
	require.NotNil(t, m.IssueID)
	base := contentRev(ctx, t, d, *m.IssueID)

	_, _, err = d.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: proj.ID, Source: "github:R_example", Actor: "sync-agent",
		Items: []db.ImportItem{{
			ExternalID: "issue-id:101", Title: "[GitHub #1] Original title", Body: "body",
			Author: "sync-agent", Status: "open",
			CreatedAt: sourceTime.Add(-time.Minute), UpdatedAt: sourceTime,
		}},
	})
	require.NoError(t, err)
	require.Equalf(t, base+1, contentRev(ctx, t, d, *m.IssueID),
		"source-owned title correction must bump content_revision")
}
