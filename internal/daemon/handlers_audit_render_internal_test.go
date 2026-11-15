package daemon

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// auditRenderStore opens a fresh store with two projects (alpha is audited,
// beta is the foreign project) and returns the store ready for cross-project
// close-audit assertions.
func auditRenderStore(t *testing.T) (*sqlitestore.Store, db.Project, db.Project) {
	t.Helper()
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	alpha, err := store.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := store.CreateProject(ctx, "beta")
	require.NoError(t, err)
	return store, alpha, beta
}

// TestDoAuditCloses_ForeignParentRendersQualified: when an audited project's
// closed child has its parent in another (active) project, the audit row must
// render the parent as "<project>#<short_id>", not the bare short_id. A bare
// suffix is ambiguous across projects and does not round-trip as a --parent
// filter against the audited project.
func TestDoAuditCloses_ForeignParentRendersQualified(t *testing.T) {
	ctx := t.Context()
	store, alpha, beta := auditRenderStore(t)

	parentBeta, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: beta.ID, Title: "foreign parent", Author: "tester",
	})
	require.NoError(t, err)
	childAlpha, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "child in alpha", Author: "tester",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: childAlpha.ID, ToIssueID: parentBeta.ID,
		Type: "parent", Author: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, childAlpha.ID, "done", "tester",
		"Closed the cross-project child after verifying the fix.", nil)
	require.NoError(t, err)

	resp, err := doAuditCloses(ctx, ServerConfig{DB: store},
		&api.AuditClosesRequest{ProjectID: alpha.ID})
	require.NoError(t, err)
	require.Len(t, resp.Body.Rows, 1)
	assert.Equal(t, "beta#"+parentBeta.ShortID, resp.Body.Rows[0].Parent,
		"foreign parent must render qualified as project#short_id")
}

// TestDoAuditCloses_SameProjectParentRendersBare: a same-project parent must
// still render as the bare short_id, with no project qualifier.
func TestDoAuditCloses_SameProjectParentRendersBare(t *testing.T) {
	ctx := t.Context()
	store, alpha, _ := auditRenderStore(t)

	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "local parent", Author: "tester",
	})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "local child", Author: "tester",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: parent.ID,
		Type: "parent", Author: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, child.ID, "done", "tester",
		"Closed the same-project child after verifying the fix.", nil)
	require.NoError(t, err)

	resp, err := doAuditCloses(ctx, ServerConfig{DB: store},
		&api.AuditClosesRequest{ProjectID: alpha.ID})
	require.NoError(t, err)
	require.Len(t, resp.Body.Rows, 1)
	assert.Equal(t, parent.ShortID, resp.Body.Rows[0].Parent,
		"same-project parent must render as a bare short_id")
}

func TestDoAuditClosesCarriesCurrentVerificationEndpointProjects(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	spoke, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hub, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	claimIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spoke.ID, Title: "completed task", Author: "worker",
	})
	require.NoError(t, err)
	reviewIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spoke.ID, Title: "review", Author: "reviewer",
	})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: claimIssue.ID, Author: "worker", Body: "Completion claim",
	})
	require.NoError(t, err)
	refutation, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: reviewIssue.ID, Author: "reviewer", Body: "Open review concern",
		ReplyToUID: claim.UID, ReplyKind: "refute",
	})
	require.NoError(t, err)
	claimIssue, _, _, err = store.CloseIssueGuarded(ctx, db.CloseIssueParams{
		IssueID: claimIssue.ID, ExpectedProjectID: spoke.ID, Actor: "worker", Reason: "done",
		Message:               "Completed the requested change and checked its focused tests.",
		Evidence:              []db.Evidence{{Type: "test", Command: "go test ./example"}},
		VerificationIssueUIDs: []string{claimIssue.UID, reviewIssue.UID},
		VerificationWarnings: []db.VerificationWarning{{Code: "open_refutations", Refutations: []db.OpenRefutation{{
			Refutation: refutation.UID, Target: claim.UID, Author: refutation.Author, Summary: refutation.Body,
		}}}},
	})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: claimIssue.ID, FromProjectID: spoke.ID, ToProjectID: hub.ID,
		IfMatchRev: claimIssue.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	reviewIssue, err = store.IssueByUID(ctx, reviewIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: reviewIssue.ID, FromProjectID: spoke.ID, ToProjectID: hub.ID,
		IfMatchRev: reviewIssue.Revision, Actor: "reviewer",
	})
	require.NoError(t, err)

	resp, err := doAuditCloses(ctx, ServerConfig{DB: store}, &api.AuditClosesRequest{ProjectID: spoke.ID})
	require.NoError(t, err)
	require.Len(t, resp.Body.Rows, 1)
	projects := resp.Body.Rows[0].VerificationEndpointProjects
	require.Len(t, projects, 2)
	for _, projectUID := range projects {
		assert.Equal(t, hub.UID, projectUID)
	}
}

func TestDoAuditCloses_AttachmentSnapshotRetainsEvidenceAndParent(t *testing.T) {
	ctx := t.Context()
	store, project, _ := auditRenderStore(t)
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Report delivery", Author: "operator"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Deliver final report", Author: "operator"})
	require.NoError(t, err)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "operator"})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: child.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.report": jsontext.Value(`{"name":"report.pdf","media_type":"application/pdf","size":12,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}`)}})
	require.NoError(t, err)
	_, _, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: child.ID, Reason: "done", Actor: "operator", Evidence: []db.Evidence{{Type: "attachment", AttachmentID: "report"}}})
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, store.DeleteLinkByID(ctx, link.ID))
	response, err := doAuditCloses(ctx, ServerConfig{DB: store}, &api.AuditClosesRequest{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, response.Body.Rows, 1)
	row := response.Body.Rows[0]
	assert.Equal(t, []string{"attachment"}, row.EvidenceTypes)
	assert.NotContains(t, row.Flags, "no-evidence")
	assert.Equal(t, parent.UID, row.ParentUID)
	assert.Equal(t, parent.ShortID, row.Parent)
	filtered, err := doAuditCloses(ctx, ServerConfig{DB: store}, &api.AuditClosesRequest{ProjectID: project.ID, NoEvidence: true})
	require.NoError(t, err)
	assert.Empty(t, filtered.Body.Rows)
}
