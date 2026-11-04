package db

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

func TestCronFleetSinceUTCYearBounds(t *testing.T) {
	for _, timestamp := range []string{"9999-12-31T23:59:59-01:00", "0000-01-01T00:00:00+01:00"} {
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		require.NoError(t, err)
		_, err = NormalizeCronRunList(CronRunList{AllProjects: true, Since: at})
		require.ErrorIs(t, err, cron.ErrInvalid)
	}
}

func drawCronFleetInput(ht *hegel.T) CronRunList {
	states := []string{"running", "succeeded", "failed", "cancelled", "unknown"}
	return CronRunList{AllProjects: true, ProjectIDs: []int64{int64(hegel.Draw(ht, hegel.Integers(1, 1000))), int64(hegel.Draw(ht, hegel.Integers(1, 1000)))}, Statuses: []string{hegel.Draw(ht, hegel.SampledFrom(states)), hegel.Draw(ht, hegel.SampledFrom(states)), hegel.Draw(ht, hegel.SampledFrom(states))}, IssueUID: strings.ToLower("01ARZ3NDEKTSV4RRFFQ69G5FA0"), Since: time.Unix(int64(hegel.Draw(ht, hegel.Integers(0, 2000000000))), 1).In(time.FixedZone("offset", 3600))}
}

func TestCronFleetNormalizationIdempotenceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		one, err := NormalizeCronRunList(drawCronFleetInput(ht))
		require.NoError(ht, err)
		two, err := NormalizeCronRunList(one)
		require.NoError(ht, err)
		require.Equal(ht, one, two)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestCronFleetNormalizationPreservesInputsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		in := drawCronFleetInput(ht)
		statuses := slices.Clone(in.Statuses)
		projects := slices.Clone(in.ProjectIDs)
		_, err := NormalizeCronRunList(in)
		require.NoError(ht, err)
		require.Equal(ht, statuses, in.Statuses)
		require.Equal(ht, projects, in.ProjectIDs)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestCronFleetFilterLabelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		label string
		valid bool
	}{
		{"x", true}, {strings.Repeat("x", 256), true}, {strings.Repeat("界", 85), true},
		{strings.Repeat("x", 257), false}, {strings.Repeat("界", 86), false}, {" \t", false}, {"\x00", false}, {string([]byte{0xff}), false},
	} {
		_, err := NormalizeCronRunList(CronRunList{AllProjects: true, ExecutorLabel: tc.label})
		if tc.valid {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, cron.ErrInvalid)
		}
	}
}
