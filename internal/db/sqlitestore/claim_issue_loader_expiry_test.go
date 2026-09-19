package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// newTimedAssignmentStore seeds a project and an issue whose assignment was
// claimed with a TTL, so the issues row carries a non-NULL
// assignment_expires_on. Returns the store, project, issue, and the expected
// expiry instant.
func newTimedAssignmentStore(t *testing.T) (*Store, context.Context, db.Project, db.Issue, time.Time) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	project, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "timed assignment subject", Author: "tester",
	})
	require.NoError(t, err)

	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "worker-a", TTL: time.Hour, Now: now,
	})
	require.NoError(t, err)
	require.NotNil(t, result.Issue.AssignmentExpiresOn)
	return d, ctx, project, issue, result.Issue.AssignmentExpiresOn.UTC()
}

// TestLookupIssueIncludingDeletedKeepsAssignmentExpiry pins that the
// delete-ladder issue loader carries assignment_expires_on. It reads the same
// row as the canonical issueSelect, so dropping the column would silently
// report a timed assignment as unexpiring while snapshotting destructive
// verbs.
func TestLookupIssueIncludingDeletedKeepsAssignmentExpiry(t *testing.T) {
	d, ctx, _, issue, wantExpiry := newTimedAssignmentStore(t)

	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	got, _, err := lookupIssueIncludingDeleted(ctx, tx, issue.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AssignmentExpiresOn,
		"lookupIssueIncludingDeleted lost assignment_expires_on")
	assert.Equal(t, wantExpiry, got.AssignmentExpiresOn.UTC())
}

// TestResolveClaimGateIssueKeepsAssignmentExpiry pins that the claim-gate
// issue loader carries assignment_expires_on, matching the canonical issue
// column order.
func TestResolveClaimGateIssueKeepsAssignmentExpiry(t *testing.T) {
	d, ctx, project, issue, wantExpiry := newTimedAssignmentStore(t)

	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	got, err := resolveClaimGateIssueTx(ctx, tx, project.ID, issue.ShortID)
	require.NoError(t, err)
	require.NotNil(t, got.AssignmentExpiresOn,
		"resolveClaimGateIssueTx lost assignment_expires_on")
	assert.Equal(t, wantExpiry, got.AssignmentExpiresOn.UTC())
}

// TestResolveClaimIssueKeepsAssignmentExpiry pins that the claim issue loader
// carries assignment_expires_on, matching the canonical issue column order.
func TestResolveClaimIssueKeepsAssignmentExpiry(t *testing.T) {
	d, ctx, project, issue, wantExpiry := newTimedAssignmentStore(t)

	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	got, _, err := resolveClaimIssueTx(ctx, tx, project.ID, issue.ShortID)
	require.NoError(t, err)
	require.NotNil(t, got.AssignmentExpiresOn,
		"resolveClaimIssueTx lost assignment_expires_on")
	assert.Equal(t, wantExpiry, got.AssignmentExpiresOn.UTC())
}
