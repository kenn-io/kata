package db_test

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueSyncBindingTimestampAlwaysAdvances(t *testing.T) {
	previous := time.Date(2026, 9, 29, 12, 0, 0, 123000000, time.UTC)
	for _, now := range []time.Time{previous.Add(-time.Hour), previous, previous.Add(time.Nanosecond)} {
		require.Equal(t, previous.Add(time.Millisecond), db.NextIssueSyncBindingUpdatedAt(previous, now))
	}
	require.Equal(t, previous.Add(time.Second), db.NextIssueSyncBindingUpdatedAt(previous, previous.Add(time.Second)))
}

func TestIssueStatusScanRejectsInvalidProgressAndConfigInjection(t *testing.T) {
	for _, config := range []string{`null`, `[]`, `{"_status_sync":null}`, `{"_status_sync":{"pending":{"after":2,"through":1}}}`, `{"_status_sync":{"sweep":{"after":-1}}}`, `{"_status_sync":{"locator_page":-1}}`, `{"_status_sync":{"unexpected":true}}`} {
		_, err := db.DecodeIssueStatusScan(jsontext.Value(config))
		require.Error(t, err)
	}
	previous := jsontext.Value(`{"status_sync":"two-way","_status_sync":{"pending":{"after":1,"through":5},"sweep":{"after":3,"through":6}}}`)
	next := jsontext.Value(`{"status_sync":"one-way","_status_sync":{"pending":{"after":99,"through":99}}}`)
	merged, err := db.PreserveIssueStatusScanConfig(previous, next)
	require.NoError(t, err)
	state, err := db.DecodeIssueStatusScan(merged)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Pending.After)
	require.Equal(t, int64(3), state.Sweep.After)
	mode, err := db.IssueStatusMode(merged)
	require.NoError(t, err)
	require.Equal(t, "one-way", mode)
	public, err := db.PublicIssueSyncConfig(merged)
	require.NoError(t, err)
	require.JSONEq(t, `{"status_sync":"one-way"}`, string(public))
}

func TestIssueSyncConfigComparisonIgnoresPrivateProgress(t *testing.T) {
	left := jsontext.Value(`{"status_sync":"two-way","title_prefix":true}`)
	right := jsontext.Value(`{"title_prefix":true,"_status_sync":{"pending":{"after":1,"through":2},"sweep":{"after":0,"through":2}},"status_sync":"two-way"}`)
	match, err := db.IssueSyncConfigMatches(left, right)
	require.NoError(t, err)
	require.True(t, match)
	match, err = db.IssueSyncConfigMatches(left, jsontext.Value(`{"status_sync":"one-way","title_prefix":true}`))
	require.NoError(t, err)
	require.False(t, match)
}
