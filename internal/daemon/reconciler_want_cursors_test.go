package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/vector"
)

func TestAdvanceWantCursorsPreservesOmittedAndSkippedProjects(t *testing.T) {
	r := Reconciler{wantCursors: map[string]string{
		"unvisited-project": "last-issue",
		"throttled-project": "page-one-last-issue",
	}}

	r.advanceWantCursors([]vector.ArtifactWant{
		{ProjectUID: "visited-project", IssueUID: "issue-1"},
		{ProjectUID: "throttled-project", IssueUID: "page-two-issue"},
	}, map[string]error{"visited-project": nil})

	require.Equal(t, "last-issue", r.wantCursors["unvisited-project"])
	require.Equal(t, "page-one-last-issue", r.wantCursors["throttled-project"])
}
