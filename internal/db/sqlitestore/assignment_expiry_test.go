package sqlitestore_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestExpireAssignments_RejectsFederatedSpokeWithPushEnabled(t *testing.T) {
	t.Parallel()
	d, ctx, project, issue := setupTestIssue(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	_, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent-one", TTL: time.Minute, Now: now,
	})
	require.NoError(t, err)
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "http://127.0.0.1:7787", HubProjectID: project.ID,
		HubProjectUID: project.UID, ReplayHorizonEventID: 1,
		Enabled: true, PushEnabled: true, Actor: "bound-agent",
	})
	require.NoError(t, err)

	events, err := d.ExpireAssignments(ctx, db.ExpireAssignmentsParams{
		ProjectID: project.ID, Now: now.Add(2 * time.Minute), Limit: 10,
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrFederatedReadOnly))
	assert.True(t, errors.Is(err, db.ErrFederatedSpokeUnsupported))
	assert.Empty(t, events)
	stored, err := d.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "agent-one", *stored.Owner)
}
