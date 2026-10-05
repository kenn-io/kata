package tui

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestDetailCreationAttribution(t *testing.T) {
	for _, state := range []string{"verified", "pending", "legacy"} {
		t.Run(state, func(t *testing.T) {
			var issue Issue
			require.NoError(t, json.Unmarshal([]byte(`{"short_id":"abc1","title":"Example issue","status":"open","author":"source-agent","source_actor":"source-agent","teammate":"example-worker","verification":"`+state+`","accountable_actor":"member-one"}`), &issue))
			model := detailModel{issue: &issue}
			text := stripANSI(strings.Join(model.documentHeader(80, viewChrome{}), "\n"))
			require.Contains(t, text, "creation: "+state)
			require.Contains(t, text, "source-agent / example-worker")
			if state == "verified" {
				require.Contains(t, text, "accountable: member-one")
			} else {
				require.NotContains(t, text, "member-one")
			}

			var comment CommentEntry
			require.NoError(t, json.Unmarshal([]byte(`{"author":"comment-agent","source_actor":"comment-agent","teammate":"comment-worker","verification":"`+state+`","accountable_actor":"member-two","body":"Example comment"}`), &comment))
			chunks := commentChunks([]CommentEntry{comment}, 80, 0, tabState{})
			text = stripANSI(strings.Join(chunks[0].lines, "\n"))
			require.Contains(t, text, "creation: "+state)
			require.Contains(t, text, "comment-agent / comment-worker")
			if state == "verified" {
				require.Contains(t, text, "accountable: member-two")
			} else {
				require.NotContains(t, text, "member-two")
			}

			for _, line := range model.documentHeader(30, viewChrome{}) {
				require.LessOrEqual(t, runewidth.StringWidth(stripANSI(line)), 32)
			}
			for _, line := range commentChunks([]CommentEntry{comment}, 32, 0, tabState{})[0].lines {
				require.LessOrEqual(t, runewidth.StringWidth(stripANSI(line)), 32)
			}
		})
	}
}

func TestDetailCreationAttributionPage(t *testing.T) {
	defer snapshotInit(t)()
	model := detailModel{
		issue: &Issue{ShortID: "abc1", Title: "Example issue", Status: "open", Author: "source-agent",
			CreatedAt: snapshotFixedNow, UpdatedAt: snapshotFixedNow,
			Verification: "verified", AccountableActor: "member-one", SourceActor: "source-agent", Teammate: "example-worker"},
		comments: []CommentEntry{{Author: "comment-agent", Teammate: "comment-worker", Body: "Example comment", CreatedAt: snapshotFixedNow,
			Verification: "pending", SourceActor: "comment-agent"}},
		activeTab: tabComments,
	}
	for _, width := range []int{80, 32} {
		page := stripANSI(model.View(width, 50, viewChrome{scope: scope{projectName: "example-project"}, version: "dev"}))
		assertLineCount(t, page, 50)
		assertLinesFitWidth(t, page, width)
		require.Contains(t, page, "creation: verified")
		require.Contains(t, page, "accountable: member-one")
		require.Contains(t, page, "creation: pending")
		require.Contains(t, page, "Example comment")
		t.Logf("%d-column detail page:\n%s", width, page)
	}
}

func TestCreationAttributionDoesNotInventProofAndSanitizesLabels(t *testing.T) {
	require.Empty(t, creationAttributionLines(db.AttributionView{}, "source-agent", "example-worker", 80))
	lines := creationAttributionLines(db.AttributionView{
		Verification: "unknown", AccountableActor: "unverified-account", SourceActor: "source\x1b[2J-agent\n",
	}, "", "example\tworker", 80)
	text := strings.Join(lines, "\n")
	require.Contains(t, text, "creation: legacy")
	require.NotContains(t, text, "unverified-account")
	require.NotContains(t, text, "\x1b")
	require.NotContains(t, text, "\t")
}
