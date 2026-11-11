package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDescribeRecurring(t *testing.T) {
	anchor := at(t, "2026-10-01T12:00:00Z")
	j := nativeJob(t)
	j.Enabled = false
	j.Catchup = "all"
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	d, err := DescribeRecurring(j, definitionUID, p, anchor.Add(5*time.Hour))
	require.NoError(t, err)
	require.Equal(t, anchor.Add(2*time.Hour), d.NextDueAt)
	require.Len(t, d.Batch.Due, 2)
}

func TestDescribeRecurringPreviewExhausted(t *testing.T) {
	j := nativeJob(t)
	last := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	progress := RecurringProgress{Version: 1, Anchor: last, Through: last}
	_, err := DescribeRecurring(j, definitionUID, progress, last)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "schedule has no supported next occurrence")
}
