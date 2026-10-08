package db_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: provider checkpoints are private, survive operator re-enable,
// and do not invalidate optimistic public-config snapshots.
func TestProviderCheckpointPrivacyAndPreservation(t *testing.T) {
	old := jsontext.Value(`{"project_id":"project-1","status_sync":"two-way","_provider_checkpoint":{"versions":{"task-1":{"hash":"example"}}},"_status_sync":{"pending":{"after":0,"through":0},"sweep":{"after":0,"through":0}}}`)
	next := jsontext.Value(`{"project_id":"project-1","status_sync":"one-way","_provider_checkpoint":{"versions":{"forged":{}}}}`)
	public, err := db.PublicIssueSyncConfig(old)
	require.NoError(t, err)
	require.NotContains(t, string(public), "_provider_checkpoint")
	require.NotContains(t, string(public), "_status_sync")
	merged, err := db.PreserveIssueStatusScanConfig(old, next)
	require.NoError(t, err)
	require.Contains(t, string(merged), "task-1")
	require.NotContains(t, string(merged), "forged")
	matches, err := db.IssueSyncConfigMatches(old, jsontext.Value(`{"project_id":"project-1","status_sync":"two-way"}`))
	require.NoError(t, err)
	require.True(t, matches)
}
