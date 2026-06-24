package githubsync

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/importlabels"
)

// IsPullRequestIssue reports whether a GitHub issues API row is a pull request.
func IsPullRequestIssue(issue Issue) bool {
	return issue.PullRequest != nil
}

// BuildImportBatch maps GitHub issue and comment API rows to kata import items.
func BuildImportBatch(sourceKey string, issues []Issue, comments map[int][]Comment, syncStartedAt time.Time) db.ImportBatchParams {
	return BuildImportBatchWithConfig(sourceKey, Config{}, issues, comments, syncStartedAt)
}

// BuildImportBatchWithConfig maps GitHub issue and comment API rows to kata
// import items using stored provider config for presentation choices.
func BuildImportBatchWithConfig(sourceKey string, config Config, issues []Issue, comments map[int][]Comment, syncStartedAt time.Time) db.ImportBatchParams {
	batch := db.ImportBatchParams{
		Source: sourceKey,
		Actor:  actorGitHubSync,
		Items:  make([]db.ImportItem, 0, len(issues)),
	}
	titlePrefix := config.UseTitlePrefix()
	for _, issue := range issues {
		if IsPullRequestIssue(issue) {
			continue
		}
		createdAt, updatedAt := normalizedIssueTimes(issue, syncStartedAt)
		item := db.ImportItem{
			ExternalID:        issueExternalID(issue),
			LegacyExternalIDs: issueLegacyExternalIDs(issue),
			Title:             issueTitle(issue, titlePrefix),
			Body:              issueBody(issue),
			Author:            userLogin(issue.User),
			Owner:             firstAssignee(issue.Assignees),
			Status:            issueStatus(issue),
			CreatedAt:         createdAt,
			UpdatedAt:         updatedAt,
			Labels:            issueLabels(issue.Labels),
			Comments:          issueComments(comments[issue.Number], issue.Number, updatedAt, syncStartedAt),
		}
		if item.Status == "closed" {
			reason := issueClosedReason(issue)
			item.ClosedReason = &reason
			closedAt := timeValue(issue.ClosedAt)
			if closedAt.IsZero() || closedAt.Before(createdAt) {
				closedAt = updatedAt
			}
			item.ClosedAt = &closedAt
		}
		batch.Items = append(batch.Items, item)
	}
	return batch
}

// issueExternalID derives the import_mappings key for a GitHub issue. The REST
// numeric id is canonical because it is permanently stable and present on every
// well-formed API row; node_id is only a fallback when id is absent. Preferring
// node_id would let the key switch from issue-id:<id> to issue:<node_id> across
// replays of the same object, defeating idempotent upsert.
func issueExternalID(issue Issue) string {
	if issue.ID != 0 {
		return fmt.Sprintf("issue-id:%d", issue.ID)
	}
	if nodeID := strings.TrimSpace(issue.NodeID); nodeID != "" {
		return "issue:" + nodeID
	}
	return fmt.Sprintf("issue-number:%d", issue.Number)
}

// issueLegacyExternalIDs returns external ids that earlier kata versions may
// have used for this issue, so import upsert can adopt a pre-upgrade mapping
// onto the canonical key. Before the REST id became canonical the key was
// issue:<node_id>; emit that alias whenever an id-keyed issue also carries a
// node_id. When id is absent the node_id key is still canonical, so there is no
// alias to migrate.
func issueLegacyExternalIDs(issue Issue) []string {
	if issue.ID == 0 {
		return nil
	}
	if nodeID := strings.TrimSpace(issue.NodeID); nodeID != "" {
		return []string{"issue:" + nodeID}
	}
	return nil
}

func issueTitle(issue Issue, titlePrefix bool) string {
	title := strings.TrimSpace(issue.Title)
	if titlePrefix {
		if title == "" {
			title = "(untitled)"
		}
		return fmt.Sprintf("[GitHub #%d] %s", issue.Number, title)
	}
	if title == "" {
		return fmt.Sprintf("(untitled GitHub issue #%d)", issue.Number)
	}
	return issue.Title
}

func issueBody(issue Issue) string {
	body := strings.TrimRight(issue.Body, "\n")
	return body + "\n---\nImported from GitHub: " + strings.TrimSpace(issue.HTMLURL)
}

func userLogin(user *User) string {
	if user == nil {
		return githubGhost
	}
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return githubGhost
	}
	return login
}

func firstAssignee(assignees []User) *string {
	if len(assignees) == 0 {
		return nil
	}
	login := strings.TrimSpace(assignees[0].Login)
	if login == "" {
		return nil
	}
	return &login
}

func normalizedIssueTimes(issue Issue, syncStartedAt time.Time) (time.Time, time.Time) {
	createdAt := timeValue(issue.CreatedAt)
	updatedAt := timeValue(issue.UpdatedAt)
	closedAt := timeValue(issue.ClosedAt)
	createdMissing := createdAt.IsZero()
	if createdAt.IsZero() && !updatedAt.IsZero() {
		createdAt = updatedAt
	}
	if updatedAt.IsZero() && !createdAt.IsZero() {
		updatedAt = createdAt
	}
	if createdAt.IsZero() && updatedAt.IsZero() {
		createdAt = syncStartedAt
		updatedAt = syncStartedAt
	}
	// When created_at was absent, a real closed_at bounds the synthesized
	// created_at from above so emitted closed items satisfy created <= closed
	// without discarding the genuine closed timestamp.
	if createdMissing && !closedAt.IsZero() && createdAt.After(closedAt) {
		createdAt = closedAt
	}
	if updatedAt.Before(createdAt) {
		updatedAt = createdAt
	}
	return createdAt, updatedAt
}

func timeValue(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func issueStatus(issue Issue) string {
	if strings.ToLower(strings.TrimSpace(issue.State)) == "closed" {
		return "closed"
	}
	return "open"
}

func issueClosedReason(issue Issue) string {
	if issue.StateReason != nil && strings.ToLower(strings.TrimSpace(*issue.StateReason)) == "not_planned" {
		return "wontfix"
	}
	return "done"
}

func issueLabels(labels []Label) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = importlabels.AppendNormalized(out, seen, label.Name)
	}
	return out
}

func issueComments(comments []Comment, issueNumber int, issueUpdatedAt, syncStartedAt time.Time) []db.ImportComment {
	out := make([]db.ImportComment, 0, len(comments))
	for i, comment := range comments {
		body := comment.Body
		if strings.TrimSpace(body) == "" {
			body = "(no content)"
		}
		createdAt := timeValue(comment.CreatedAt)
		if createdAt.IsZero() {
			createdAt = issueUpdatedAt
		}
		if createdAt.IsZero() {
			createdAt = syncStartedAt
		}
		out = append(out, db.ImportComment{
			ExternalID:        commentExternalID(comment, issueNumber, i),
			LegacyExternalIDs: commentLegacyExternalIDs(comment),
			Author:            userLogin(comment.User),
			Body:              body,
			CreatedAt:         createdAt,
		})
	}
	return out
}

// commentExternalID mirrors issueExternalID: the REST numeric id is the
// canonical, stable key and node_id is only a fallback when id is absent.
func commentExternalID(comment Comment, issueNumber, index int) string {
	if comment.ID != 0 {
		return fmt.Sprintf("comment-id:%d", comment.ID)
	}
	if nodeID := strings.TrimSpace(comment.NodeID); nodeID != "" {
		return "comment:" + nodeID
	}
	return fmt.Sprintf("comment-missing:%d:%d", issueNumber, index)
}

// commentLegacyExternalIDs mirrors issueLegacyExternalIDs: the pre-upgrade
// comment key was comment:<node_id>.
func commentLegacyExternalIDs(comment Comment) []string {
	if comment.ID == 0 {
		return nil
	}
	if nodeID := strings.TrimSpace(comment.NodeID); nodeID != "" {
		return []string{"comment:" + nodeID}
	}
	return nil
}
