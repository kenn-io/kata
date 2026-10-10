package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCronMaterializationNeeded(t *testing.T) {
	events := []FoldEvent{
		{UID: "ISSUE", Type: "issue.created"},
		{UID: "JOB", Type: "cron.job.created"},
		{UID: "RUN", Type: "cron.run.observed"},
	}
	cases := []struct {
		name     string
		accepted []string
		want     bool
	}{
		{name: "full rebuild", accepted: nil, want: true},
		{name: "issue-only batch", accepted: []string{"ISSUE"}, want: false},
		{name: "definition batch", accepted: []string{"ISSUE", "JOB"}, want: true},
		{name: "run batch", accepted: []string{"RUN"}, want: true},
		{name: "generated audit event outside fold", accepted: []string{"AUDIT"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CronMaterializationNeeded(events, tc.accepted))
		})
	}
}
