package githubsync

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestBuildImportBatch(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	createdAt := mustTime(t, "2026-06-20T10:00:00Z")
	updatedAt := mustTime(t, "2026-06-21T10:00:00Z")
	closedUpdatedAt := mustTime(t, "2026-06-21T12:00:00Z")
	closedAt := mustTime(t, "2026-06-21T13:00:00Z")

	notPlanned := "not_planned"

	issues := []Issue{
		{
			NodeID:    "I_open",
			Number:    123,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/123",
			Title:     "   ",
			Body:      "Issue body\n",
			State:     "open",
			User:      nil,
			CreatedAt: nil,
			UpdatedAt: &updatedAt,
			Labels: []Label{
				{Name: "Needs Review!"},
				{Name: "needs review"},
				{Name: "bad label @#$"},
			},
			Assignees: []User{
				{Login: "first-owner"},
				{Login: "second-owner"},
			},
		},
		{
			NodeID:    "I_missing_times",
			Number:    124,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/124",
			Title:     "No timestamps",
			State:     "open",
			User:      &User{Login: "alice"},
			CreatedAt: nil,
			UpdatedAt: nil,
		},
		{
			NodeID:      "I_not_planned",
			Number:      125,
			HTMLURL:     "https://github.com/example-org/example-repo/issues/125",
			Title:       "Won't do",
			State:       "closed",
			StateReason: &notPlanned,
			User:        &User{Login: "bob"},
			CreatedAt:   &createdAt,
			UpdatedAt:   &closedUpdatedAt,
			ClosedAt:    nil,
		},
		{
			NodeID:    "I_done",
			Number:    126,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/126",
			Title:     "Done",
			State:     "closed",
			User:      &User{Login: "carol"},
			CreatedAt: &createdAt,
			UpdatedAt: nil,
			ClosedAt:  &closedAt,
		},
		{
			NodeID:      "I_pr",
			Number:      127,
			HTMLURL:     "https://github.com/example-org/example-repo/pull/127",
			Title:       "Pull request",
			State:       "open",
			User:        &User{Login: "dan"},
			CreatedAt:   &createdAt,
			UpdatedAt:   &updatedAt,
			PullRequest: &PullRequest{},
		},
	}

	comments := map[int][]Comment{
		123: {
			{NodeID: "C_empty", Body: "   ", User: nil, CreatedAt: nil},
			{NodeID: "C_normal", Body: "Comment body", User: &User{Login: "commenter"}, CreatedAt: &closedAt},
		},
		127: {
			{NodeID: "C_pr", Body: "PR comment", User: &User{Login: "reviewer"}, CreatedAt: &closedAt},
		},
	}

	batch := BuildImportBatch("github:repo-node", issues, comments, syncStartedAt)

	assert.Equal(t, "github:repo-node", batch.Source)
	assert.Equal(t, "github-sync", batch.Actor)
	require.Len(t, batch.Items, 4)
	assert.False(t, IsPullRequestIssue(issues[0]))
	assert.True(t, IsPullRequestIssue(issues[4]))

	open := itemByExternalID(t, batch.Items, "issue:I_open")
	assert.Equal(t, "[GitHub #123] (untitled)", open.Title)
	assert.Equal(t, "github-ghost", open.Author)
	require.NotNil(t, open.Owner)
	assert.Equal(t, "first-owner", *open.Owner)
	assert.Equal(t, "open", open.Status)
	assert.Nil(t, open.ClosedReason)
	assert.Nil(t, open.ClosedAt)
	assert.Equal(t, updatedAt, open.CreatedAt)
	assert.Equal(t, updatedAt, open.UpdatedAt)
	assert.Equal(t, []string{"needs-review", "bad-label"}, open.Labels)
	bodyParts := strings.Split(open.Body, "\n---\n")
	require.Len(t, bodyParts, 2)
	assert.Equal(t, "Issue body", bodyParts[0])
	assert.Equal(t, "Imported from GitHub: https://github.com/example-org/example-repo/issues/123", bodyParts[1])
	require.Len(t, open.Comments, 2)
	assert.Equal(t, "comment:C_empty", open.Comments[0].ExternalID)
	assert.Equal(t, "github-ghost", open.Comments[0].Author)
	assert.Equal(t, "(no content)", open.Comments[0].Body)
	assert.Equal(t, updatedAt, open.Comments[0].CreatedAt)
	assert.Equal(t, "comment:C_normal", open.Comments[1].ExternalID)
	assert.Equal(t, "commenter", open.Comments[1].Author)
	assert.Equal(t, "Comment body", open.Comments[1].Body)
	assert.Equal(t, closedAt, open.Comments[1].CreatedAt)

	missingTimes := itemByExternalID(t, batch.Items, "issue:I_missing_times")
	assert.Equal(t, "[GitHub #124] No timestamps", missingTimes.Title)
	assert.Equal(t, syncStartedAt, missingTimes.CreatedAt)
	assert.Equal(t, syncStartedAt, missingTimes.UpdatedAt)

	wontfix := itemByExternalID(t, batch.Items, "issue:I_not_planned")
	assert.Equal(t, "[GitHub #125] Won't do", wontfix.Title)
	assert.Equal(t, "closed", wontfix.Status)
	require.NotNil(t, wontfix.ClosedReason)
	assert.Equal(t, "wontfix", *wontfix.ClosedReason)
	require.NotNil(t, wontfix.ClosedAt)
	assert.Equal(t, closedUpdatedAt, *wontfix.ClosedAt)

	done := itemByExternalID(t, batch.Items, "issue:I_done")
	assert.Equal(t, "[GitHub #126] Done", done.Title)
	assert.Equal(t, createdAt, done.CreatedAt)
	assert.Equal(t, createdAt, done.UpdatedAt)
	assert.Equal(t, "closed", done.Status)
	require.NotNil(t, done.ClosedReason)
	assert.Equal(t, "done", *done.ClosedReason)
	require.NotNil(t, done.ClosedAt)
	assert.Equal(t, closedAt, *done.ClosedAt)

	for _, item := range batch.Items {
		assert.NotEqual(t, "issue:I_pr", item.ExternalID)
		for _, comment := range item.Comments {
			assert.NotEqual(t, "comment:C_pr", comment.ExternalID)
		}
	}
}

func TestBuildImportBatchCanPreserveGitHubTitles(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	updatedAt := mustTime(t, "2026-06-21T10:00:00Z")
	titlePrefix := false
	issues := []Issue{
		{
			NodeID:    "I_normal",
			Number:    201,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/201",
			Title:     "Original title",
			State:     "open",
			User:      &User{Login: "alice"},
			CreatedAt: &updatedAt,
			UpdatedAt: &updatedAt,
		},
		{
			NodeID:    "I_empty",
			Number:    202,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/202",
			Title:     "   ",
			State:     "open",
			User:      &User{Login: "bob"},
			CreatedAt: &updatedAt,
			UpdatedAt: &updatedAt,
		},
	}

	batch := BuildImportBatchWithConfig("github:repo-node", Config{TitlePrefix: &titlePrefix}, issues, nil, syncStartedAt)

	assert.Equal(t, "Original title", itemByExternalID(t, batch.Items, "issue:I_normal").Title)
	assert.Equal(t, "(untitled GitHub issue #202)", itemByExternalID(t, batch.Items, "issue:I_empty").Title)
}

func TestBuildImportBatchUsesExternalIDFallbacks(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	issues := mustDecodeJSON[[]Issue](t, `[
		{
			"id": 9876543210,
			"number": 201,
			"html_url": "https://github.com/example-org/example-repo/issues/201",
			"title": "REST id fallback",
			"state": "open",
			"user": {"login": "alice"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		},
		{
			"number": 202,
			"html_url": "https://github.com/example-org/example-repo/issues/202",
			"title": "Number fallback",
			"state": "open",
			"user": {"login": "bob"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		}
	]`)
	comments := map[int][]Comment{
		201: mustDecodeJSON[[]Comment](t, `[
			{
				"id": 7654321,
				"body": "REST comment id",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:00:00Z"
			},
			{
				"body": "first malformed comment",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:01:00Z"
			},
			{
				"body": "second malformed comment",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:02:00Z"
			}
		]`),
	}

	batch := BuildImportBatch("github:repo-node", issues, comments, syncStartedAt)

	restID := itemByExternalID(t, batch.Items, "issue-id:9876543210")
	assert.NotEqual(t, "issue:", restID.ExternalID)
	require.Len(t, restID.Comments, 3)
	assert.Equal(t, "comment-id:7654321", restID.Comments[0].ExternalID)
	assert.Equal(t, "comment-missing:201:1", restID.Comments[1].ExternalID)
	assert.Equal(t, "comment-missing:201:2", restID.Comments[2].ExternalID)
	assert.NotEqual(t, restID.Comments[1].ExternalID, restID.Comments[2].ExternalID)

	numberFallback := itemByExternalID(t, batch.Items, "issue-number:202")
	assert.NotEqual(t, "issue:", numberFallback.ExternalID)
}

func TestBuildImportBatchExternalIDStableAcrossNodeID(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	withoutNodeID := mustDecodeJSON[[]Issue](t, `[
		{
			"id": 9876543210,
			"number": 201,
			"html_url": "https://github.com/example-org/example-repo/issues/201",
			"title": "Stable id",
			"state": "open",
			"user": {"login": "alice"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		}
	]`)
	withNodeID := mustDecodeJSON[[]Issue](t, `[
		{
			"id": 9876543210,
			"node_id": "I_kwDOABCDEF",
			"number": 201,
			"html_url": "https://github.com/example-org/example-repo/issues/201",
			"title": "Stable id",
			"state": "open",
			"user": {"login": "alice"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		}
	]`)
	commentsWithoutNodeID := map[int][]Comment{
		201: mustDecodeJSON[[]Comment](t, `[
			{
				"id": 7654321,
				"body": "Stable comment",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:00:00Z"
			}
		]`),
	}
	commentsWithNodeID := map[int][]Comment{
		201: mustDecodeJSON[[]Comment](t, `[
			{
				"id": 7654321,
				"node_id": "IC_kwDOABCDEF",
				"body": "Stable comment",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:00:00Z"
			}
		]`),
	}

	first := BuildImportBatch("github:repo-node", withoutNodeID, commentsWithoutNodeID, syncStartedAt)
	second := BuildImportBatch("github:repo-node", withNodeID, commentsWithNodeID, syncStartedAt)

	require.Len(t, first.Items, 1)
	require.Len(t, second.Items, 1)
	assert.Equal(t, first.Items[0].ExternalID, second.Items[0].ExternalID,
		"external id must not switch when node_id appears on a later replay")
	assert.Equal(t, "issue-id:9876543210", second.Items[0].ExternalID)

	require.Len(t, first.Items[0].Comments, 1)
	require.Len(t, second.Items[0].Comments, 1)
	assert.Equal(t, first.Items[0].Comments[0].ExternalID, second.Items[0].Comments[0].ExternalID,
		"comment external id must not switch when node_id appears on a later replay")
	assert.Equal(t, "comment-id:7654321", second.Items[0].Comments[0].ExternalID)
}

func TestBuildImportBatchEmitsLegacyExternalIDs(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	issues := mustDecodeJSON[[]Issue](t, `[
		{
			"id": 9876543210,
			"node_id": "I_kwDOABCDEF",
			"number": 201,
			"html_url": "https://github.com/example-org/example-repo/issues/201",
			"title": "Has node id",
			"state": "open",
			"user": {"login": "alice"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		},
		{
			"id": 1234567890,
			"number": 202,
			"html_url": "https://github.com/example-org/example-repo/issues/202",
			"title": "No node id",
			"state": "open",
			"user": {"login": "bob"},
			"created_at": "2026-06-20T10:00:00Z",
			"updated_at": "2026-06-20T11:00:00Z"
		}
	]`)
	comments := map[int][]Comment{
		201: mustDecodeJSON[[]Comment](t, `[
			{
				"id": 7654321,
				"node_id": "IC_kwDOABCDEF",
				"body": "Has node id",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:00:00Z"
			},
			{
				"id": 7654322,
				"body": "No node id",
				"user": {"login": "commenter"},
				"created_at": "2026-06-20T12:01:00Z"
			}
		]`),
	}

	batch := BuildImportBatch("github:repo-node", issues, comments, syncStartedAt)

	withNode := itemByExternalID(t, batch.Items, "issue-id:9876543210")
	assert.Equal(t, []string{"issue:I_kwDOABCDEF"}, withNode.LegacyExternalIDs,
		"issue carrying both id and node_id must offer the pre-upgrade node_id key as a legacy alias")
	require.Len(t, withNode.Comments, 2)
	assert.Equal(t, []string{"comment:IC_kwDOABCDEF"}, withNode.Comments[0].LegacyExternalIDs)
	assert.Nil(t, withNode.Comments[1].LegacyExternalIDs)

	withoutNode := itemByExternalID(t, batch.Items, "issue-id:1234567890")
	assert.Nil(t, withoutNode.LegacyExternalIDs)
}

func TestBuildImportBatchClampsInvalidTimestamps(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	createdAt := mustTime(t, "2026-06-22T12:00:00Z")
	beforeCreatedAt := mustTime(t, "2026-06-22T11:00:00Z")
	afterCreatedAt := mustTime(t, "2026-06-22T13:00:00Z")

	issues := []Issue{
		{
			NodeID:    "I_inverted_times",
			Number:    301,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/301",
			Title:     "Inverted timestamps",
			State:     "open",
			User:      &User{Login: "alice"},
			CreatedAt: &createdAt,
			UpdatedAt: &beforeCreatedAt,
		},
		{
			NodeID:    "I_inverted_closed_at",
			Number:    302,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/302",
			Title:     "Inverted closed timestamp",
			State:     "closed",
			User:      &User{Login: "bob"},
			CreatedAt: &createdAt,
			UpdatedAt: &afterCreatedAt,
			ClosedAt:  &beforeCreatedAt,
		},
	}

	batch := BuildImportBatch("github:repo-node", issues, nil, syncStartedAt)

	invertedTimes := itemByExternalID(t, batch.Items, "issue:I_inverted_times")
	assert.Equal(t, createdAt, invertedTimes.CreatedAt)
	assert.Equal(t, createdAt, invertedTimes.UpdatedAt)

	invertedClosedAt := itemByExternalID(t, batch.Items, "issue:I_inverted_closed_at")
	assert.Equal(t, createdAt, invertedClosedAt.CreatedAt)
	assert.Equal(t, afterCreatedAt, invertedClosedAt.UpdatedAt)
	require.NotNil(t, invertedClosedAt.ClosedAt)
	assert.Equal(t, afterCreatedAt, *invertedClosedAt.ClosedAt)
}

func TestBuildImportBatchClosedIssueMissingCreatedAt(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	closedAt := mustTime(t, "2026-06-21T13:00:00Z")
	updatedAfterClosed := mustTime(t, "2026-06-21T14:00:00Z")

	issues := []Issue{
		{
			NodeID:    "I_closed_missing_created",
			Number:    501,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/501",
			Title:     "Closed without created_at",
			State:     "closed",
			User:      &User{Login: "alice"},
			CreatedAt: nil,
			UpdatedAt: &updatedAfterClosed,
			ClosedAt:  &closedAt,
		},
		{
			NodeID:    "I_closed_all_missing",
			Number:    502,
			HTMLURL:   "https://github.com/example-org/example-repo/issues/502",
			Title:     "Closed with only closed_at",
			State:     "closed",
			User:      &User{Login: "bob"},
			CreatedAt: nil,
			UpdatedAt: nil,
			ClosedAt:  &closedAt,
		},
	}

	batch := BuildImportBatch("github:repo-node", issues, nil, syncStartedAt)

	missingCreated := itemByExternalID(t, batch.Items, "issue:I_closed_missing_created")
	assert.Equal(t, closedAt, missingCreated.CreatedAt)
	assert.Equal(t, updatedAfterClosed, missingCreated.UpdatedAt)
	require.NotNil(t, missingCreated.ClosedAt)
	assert.Equal(t, closedAt, *missingCreated.ClosedAt)
	assert.False(t, missingCreated.ClosedAt.Before(missingCreated.CreatedAt))

	allMissing := itemByExternalID(t, batch.Items, "issue:I_closed_all_missing")
	assert.Equal(t, closedAt, allMissing.CreatedAt)
	require.NotNil(t, allMissing.ClosedAt)
	assert.Equal(t, closedAt, *allMissing.ClosedAt)
	assert.False(t, allMissing.ClosedAt.Before(allMissing.CreatedAt))
}

func TestBuildImportBatchComparesStateCaseInsensitively(t *testing.T) {
	syncStartedAt := mustTime(t, "2026-06-22T10:00:00Z")
	createdAt := mustTime(t, "2026-06-20T10:00:00Z")
	updatedAt := mustTime(t, "2026-06-21T10:00:00Z")
	stateReason := " NOT_PLANNED "
	issue := Issue{
		NodeID:      "I_case",
		Number:      401,
		HTMLURL:     "https://github.com/example-org/example-repo/issues/401",
		Title:       "Case-insensitive state",
		State:       " Closed ",
		StateReason: &stateReason,
		User:        &User{Login: "alice"},
		CreatedAt:   &createdAt,
		UpdatedAt:   &updatedAt,
		ClosedAt:    &updatedAt,
	}

	batch := BuildImportBatch("github:repo-node", []Issue{issue}, nil, syncStartedAt)

	item := itemByExternalID(t, batch.Items, "issue:I_case")
	assert.Equal(t, "closed", item.Status)
	require.NotNil(t, item.ClosedReason)
	assert.Equal(t, "wontfix", *item.ClosedReason)
}

func itemByExternalID(t *testing.T, items []db.ImportItem, externalID string) db.ImportItem {
	t.Helper()
	for _, item := range items {
		if item.ExternalID == externalID {
			return item
		}
	}
	t.Fatalf("item %q not found in %#v", externalID, items)
	return db.ImportItem{}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return ts
}

func mustDecodeJSON[T any](t *testing.T, data string) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal([]byte(data), &out))
	return out
}
