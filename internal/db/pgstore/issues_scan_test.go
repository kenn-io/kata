package pgstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// fakeIssueRow feeds a fixed column list into the destinations that
// issueDestinations produces, so the mapping from issueSelect position to
// db.Issue field is asserted without a database.
type fakeIssueRow struct{ columns []any }

func (r fakeIssueRow) Scan(dest ...any) error {
	if len(dest) != len(r.columns) {
		return assertLenMismatch(len(dest), len(r.columns))
	}
	for i, value := range r.columns {
		if value == nil {
			switch target := dest[i].(type) {
			case **string:
				*target = nil
			case **int64:
				*target = nil
			default:
				scanner, ok := dest[i].(interface{ Scan(any) error })
				if !ok {
					return assertUnsupported(i)
				}
				if err := scanner.Scan(nil); err != nil {
					return err
				}
			}
			continue
		}
		switch target := dest[i].(type) {
		case *int64:
			*target = value.(int64)
		case *string:
			*target = value.(string)
		case **int64:
			number := value.(int64)
			*target = &number
		case **string:
			text := value.(string)
			*target = &text
		case *db.JSONBlob:
			*target = db.JSONBlob(value.(string))
		default:
			if scanner, ok := dest[i].(interface{ Scan(any) error }); ok {
				if err := scanner.Scan(value); err != nil {
					return err
				}
				continue
			}
			return assertUnsupported(i)
		}
	}
	return nil
}

func assertLenMismatch(got, want int) error {
	return fmt.Errorf("scan got %d destinations, want %d", got, want)
}

func assertUnsupported(index int) error {
	return fmt.Errorf("unsupported destination at index %d", index)
}

func TestIssueDestinationsMapsSelectPositionsToFields(t *testing.T) {
	var issue db.Issue
	var assignmentExpiresOn, closedAt, deletedAt storedNullTime
	dest := issueDestinations(&issue, &assignmentExpiresOn, &closedAt, &deletedAt)
	require.Len(t, dest, 21, "issueSelect projects twenty-one columns")

	row := fakeIssueRow{columns: []any{
		int64(701), "issue-uid-2", int64(703), "project-uid-4", "short-5",
		"title-6", "body-7", "closed", "completed", "owner-10",
		"2026-05-23T12:30:00.000Z", int64(712), "author-13", `{"column":14}`, int64(715), int64(716),
		"occurrence-17", "2026-05-23T12:00:00.000Z", "2026-05-23T13:01:00.000Z",
		"2026-05-24T14:02:00.000Z", "2026-05-25T15:03:00.000Z",
	}}
	require.NoError(t, row.Scan(dest...))
	issue.AssignmentExpiresOn = assignmentExpiresOn.Time
	issue.ClosedAt = closedAt.Time
	issue.DeletedAt = deletedAt.Time

	assert.Equal(t, int64(701), issue.ID)
	assert.Equal(t, "issue-uid-2", issue.UID)
	assert.Equal(t, int64(703), issue.ProjectID)
	assert.Equal(t, "project-uid-4", issue.ProjectUID)
	assert.Equal(t, "short-5", issue.ShortID)
	assert.Equal(t, "title-6", issue.Title)
	assert.Equal(t, "body-7", issue.Body)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "completed", *issue.ClosedReason)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "owner-10", *issue.Owner)
	require.NotNil(t, issue.AssignmentExpiresOn)
	assert.Equal(t, time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC), *issue.AssignmentExpiresOn)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(712), *issue.Priority)
	assert.Equal(t, "author-13", issue.Author)
	assert.Equal(t, db.JSONBlob(`{"column":14}`), issue.Metadata)
	assert.Equal(t, int64(715), issue.Revision)
	require.NotNil(t, issue.RecurrenceID)
	assert.Equal(t, int64(716), *issue.RecurrenceID)
	require.NotNil(t, issue.OccurrenceKey)
	assert.Equal(t, "occurrence-17", *issue.OccurrenceKey)
	assert.True(t, issue.CreatedAt.Equal(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)))
	assert.True(t, issue.UpdatedAt.Equal(time.Date(2026, 5, 23, 13, 1, 0, 0, time.UTC)))
	require.NotNil(t, issue.ClosedAt)
	assert.True(t, issue.ClosedAt.Equal(time.Date(2026, 5, 24, 14, 2, 0, 0, time.UTC)))
	require.NotNil(t, issue.DeletedAt)
	assert.True(t, issue.DeletedAt.Equal(time.Date(2026, 5, 25, 15, 3, 0, 0, time.UTC)))
}
