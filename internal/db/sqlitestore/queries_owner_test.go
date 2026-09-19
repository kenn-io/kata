package sqlitestore_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestUpdateOwner_AssignFromNil(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	owner := "alice"
	updated, evt, changed, err := d.UpdateOwner(ctx, i.ID, &owner, "tester")
	require.NoError(t, err)
	assert.True(t, changed)
	require.NotNil(t, updated.Owner)
	assert.Equal(t, "alice", *updated.Owner)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.assigned", evt.Type)
	var payload struct {
		Owner     string `json:"owner"`
		UpdatedAt string `json:"updated_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &payload))
	assert.Equal(t, "alice", payload.Owner)
	assert.Equal(t, updated.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), payload.UpdatedAt)
}

func TestUpdateOwner_UnassignFromValue(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "alice")

	updated, evt, changed, err := d.UpdateOwner(ctx, i.ID, nil, "tester")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Nil(t, updated.Owner)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.unassigned", evt.Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &payload))
	assert.Contains(t, payload, "owner")
	assert.Nil(t, payload["owner"])
}

func TestUpdateOwner_NoOpSameOwner(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "alice")

	owner := "alice"
	_, evt, changed, err := d.UpdateOwner(ctx, i.ID, &owner, "tester")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

func TestUpdateOwner_NoOpAlreadyUnassigned(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	_, evt, changed, err := d.UpdateOwner(ctx, i.ID, nil, "tester")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

func TestUnassignOwner_ExpectedOwnerMatches(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent-a")
	expected := "agent-a"

	updated, evt, changed, err := d.UnassignOwner(ctx, i.ID, "tester", &expected)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.Nil(t, updated.Owner)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.unassigned", evt.Type)
}

func TestUnassignOwner_ExpectedOwnerMismatch(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent-b")
	expected := "agent-a"

	current, evt, changed, err := d.UnassignOwner(ctx, i.ID, "tester", &expected)

	require.ErrorIs(t, err, db.ErrOwnerMismatch)
	require.NotNil(t, current.Owner)
	assert.Equal(t, "agent-b", *current.Owner)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

func TestUnassignOwner_ExpectedOwnerMismatchWhenUnowned(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)
	expected := "agent-a"

	current, evt, changed, err := d.UnassignOwner(ctx, i.ID, "tester", &expected)

	require.ErrorIs(t, err, db.ErrOwnerMismatch)
	assert.Nil(t, current.Owner)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

// Regression: %q-encoded payloads produced invalid JSON for owner strings
// containing control bytes (e.g. NUL), tripping the events.payload
// json_valid CHECK and rolling back the assignment. Now built via
// encoding/json so any schema-accepted owner value round-trips cleanly.
func TestUpdateOwner_ControlByteOwnerProducesValidJSON(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	owner := "alice\x00bob"
	updated, evt, changed, err := d.UpdateOwner(ctx, i.ID, &owner, "tester")
	require.NoError(t, err)
	assert.True(t, changed)
	require.NotNil(t, updated.Owner)
	assert.Equal(t, owner, *updated.Owner)
	require.NotNil(t, evt)

	var payload struct {
		Owner string `json:"owner"`
	}
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &payload))
	assert.Equal(t, owner, payload.Owner)
}

// ClaimOwner tests

func TestClaimOwner_UnownedIssue(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent1"})
	require.NoError(t, err)
	assert.True(t, result.Changed)
	require.NotNil(t, result.Issue.Owner)
	assert.Equal(t, "agent1", *result.Issue.Owner)
	require.NotNil(t, result.Event)
	assert.Equal(t, "issue.assigned", result.Event.Type)
	assert.Nil(t, result.PreviousOwner)
}

func TestClaimOwner_AlreadyOwnedBySameActor(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent1"})
	require.NoError(t, err)
	assert.False(t, result.Changed, "claiming own issue is no-op")
	assert.Nil(t, result.Event)
	require.NotNil(t, result.Issue.Owner)
	assert.Equal(t, "agent1", *result.Issue.Owner)
}

func TestClaimOwner_IfUnownedAlreadyOwnedBySameActor(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent1", IfUnowned: true})

	require.ErrorIs(t, err, db.ErrAlreadyAssigned)
	require.NotNil(t, result.CurrentOwner)
	assert.Equal(t, "agent1", *result.CurrentOwner)
	assert.False(t, result.Changed)
	assert.Nil(t, result.Event)
}

func TestClaimOwner_AlreadyOwnedByDifferentActor(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent2"})
	require.ErrorIs(t, err, db.ErrAlreadyAssigned)
	require.NotNil(t, result.CurrentOwner)
	assert.Equal(t, "agent1", *result.CurrentOwner)
}

func TestClaimOwner_ForceReassign(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent2", Force: true})
	require.NoError(t, err)
	assert.True(t, result.Changed)
	require.NotNil(t, result.Issue.Owner)
	assert.Equal(t, "agent2", *result.Issue.Owner)
	require.NotNil(t, result.PreviousOwner)
	assert.Equal(t, "agent1", *result.PreviousOwner)
}

func TestClaimOwner_ReadOnlyFederatedSpokeRejected(t *testing.T) {
	d, ctx, p, i := setupTestIssue(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7787",
		HubProjectID:         p.ID,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
		PushEnabled:          false,
	})
	require.NoError(t, err)

	_, err = d.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: i.ID, Actor: "agent1"})
	require.ErrorIs(t, err, db.ErrFederatedReadOnly)
}

func TestClaimOwner_TimedAssignmentRenewsAndPermanentRetryPreservesExpiry(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	acquired, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent1", TTL: 30 * time.Minute, Now: now,
	})
	require.NoError(t, err)
	require.NotNil(t, acquired.Issue.AssignmentExpiresOn)
	assert.Equal(t, now.Add(30*time.Minute), acquired.Issue.AssignmentExpiresOn.UTC())
	require.Len(t, acquired.Events, 1)
	assert.Equal(t, "issue.assigned", acquired.Events[0].Type)
	require.NotNil(t, acquired.Event)
	assert.Equal(t, acquired.Events[0].UID, acquired.Event.UID)

	renewed, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent1", TTL: 30 * time.Minute, Now: now.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	require.NotNil(t, renewed.Issue.AssignmentExpiresOn)
	assert.Equal(t, now.Add(35*time.Minute), renewed.Issue.AssignmentExpiresOn.UTC())
	require.Len(t, renewed.Events, 1)
	assert.Equal(t, "issue.assignment_renewed", renewed.Events[0].Type)

	retried, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent1", Now: now.Add(6 * time.Minute),
	})
	require.NoError(t, err)
	assert.False(t, retried.Changed)
	require.NotNil(t, retried.Issue.AssignmentExpiresOn)
	assert.Equal(t, now.Add(35*time.Minute), retried.Issue.AssignmentExpiresOn.UTC())
}

func TestClaimOwner_ExpiredAssignmentTakeoverEmitsOrderedEvents(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	_, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent1", TTL: time.Minute, Now: now,
	})
	require.NoError(t, err)

	taken, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent2", IfUnowned: true,
		TTL: time.Minute, Now: now.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, taken.Changed)
	require.Len(t, taken.Events, 2)
	assert.Equal(t, "issue.assignment_expired", taken.Events[0].Type)
	assert.Equal(t, "issue.assigned", taken.Events[1].Type)
	require.NotNil(t, taken.Event)
	assert.Equal(t, taken.Events[1].UID, taken.Event.UID)
	require.NotNil(t, taken.PreviousOwner)
	assert.Equal(t, "agent1", *taken.PreviousOwner)
	require.NotNil(t, taken.Issue.Owner)
	assert.Equal(t, "agent2", *taken.Issue.Owner)
	require.NotNil(t, taken.Issue.AssignmentExpiresOn)
	assert.Equal(t, now.Add(3*time.Minute), taken.Issue.AssignmentExpiresOn.UTC())
}

func TestClaimOwner_ForcePermanentAssignmentClearsExpiry(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	_, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent1", TTL: time.Hour, Now: now,
	})
	require.NoError(t, err)

	forced, err := d.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "agent2", Force: true, Now: now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Nil(t, forced.Issue.AssignmentExpiresOn)
}
