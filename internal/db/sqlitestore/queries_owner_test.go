package sqlitestore_test

import (
	"encoding/json"
	"testing"

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

	result, err := d.ClaimOwner(ctx, i.ID, "agent1", false)
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

	result, err := d.ClaimOwner(ctx, i.ID, "agent1", false)
	require.NoError(t, err)
	assert.False(t, result.Changed, "claiming own issue is no-op")
	assert.Nil(t, result.Event)
	require.NotNil(t, result.Issue.Owner)
	assert.Equal(t, "agent1", *result.Issue.Owner)
}

func TestClaimOwner_AlreadyOwnedByDifferentActor(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, i.ID, "agent2", false)
	require.ErrorIs(t, err, db.ErrAlreadyClaimed)
	require.NotNil(t, result.CurrentOwner)
	assert.Equal(t, "agent1", *result.CurrentOwner)
}

func TestClaimOwner_ForceReassign(t *testing.T) {
	d, ctx, _, i := setupAssignedIssue(t, "agent1")

	result, err := d.ClaimOwner(ctx, i.ID, "agent2", true)
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

	_, err = d.ClaimOwner(ctx, i.ID, "agent1", false)
	require.ErrorIs(t, err, db.ErrFederatedReadOnly)
}
