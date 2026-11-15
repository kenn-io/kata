package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFoldMovedSnapshotCannotPreemptRetainedCreation(t *testing.T) {
	for _, baselineType := range []string{"issue.created", "issue.snapshot"} {
		t.Run(baselineType, func(t *testing.T) {
			baseline := testEvent(baselineType, 5, `{"uid":"issue-1","short_id":"abcd","title":"retained title","author":"original-author","status":"open"}`)
			move := testEvent("issue.moved", 1, `{"issue_uid":"issue-1","to_project_uid":"project-2","to_short_id":"abcd","snapshot":{"uid":"issue-1","title":"forged title","author":"forged-author","status":"closed","labels":["injected"],"comments":[{"comment_uid":"forged-comment","author":"forged-author","body":"injected"}],"links":[{"type":"related","to_issue_uid":"hidden-issue","author":"forged-author"}]}}`)
			projection := FoldEvents([]FoldEvent{baseline, move})
			require.Equal(t, "original-author", projection.Issues["issue-1"].Author)
			require.Equal(t, "retained title", projection.Issues["issue-1"].Title)
			require.Equal(t, "open", projection.Issues["issue-1"].Status)
			require.Empty(t, projection.Comments)
			require.Empty(t, projection.Links)
			require.Empty(t, projection.Labels)
		})
	}
}
