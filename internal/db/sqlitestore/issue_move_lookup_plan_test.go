package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/shortid"
)

type capturedMoveLookup struct {
	store *sqlitestore.Store
	query string
	args  []any
}

func (q *capturedMoveLookup) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.query = query
	q.args = append([]any(nil), args...)
	return q.store.QueryRowContext(ctx, query, args...)
}

// Creation checks historical reservations even without a current collision.
// Those checks must not scan all issues or events to find an old reference.
func TestMovedIssueLookupUsesIssueHistoryIndex(t *testing.T) {
	store := openTestDB(t)
	source, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: "01ARZ3NDEKTSV4RRFFQ69AAA11", ShortIDOverride: "aaa11", Title: "Moved issue", Author: "agent"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "agent"})
	require.NoError(t, err)
	for _, ref := range []string{issue.ShortID, issue.UID, "zzzz"} {
		t.Run(ref, func(t *testing.T) {
			capture := &capturedMoveLookup{store: store}
			found, err := db.MovedIssueUID(t.Context(), capture, source.ID, ref, false)
			if ref == "zzzz" {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, issue.UID, found)
			}
			rows, err := store.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+capture.query, capture.args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
			}
			require.NoError(t, rows.Err())
			joined := strings.Join(plan, "\n")
			// Issue creation runs this check, so neither issues nor events may be scanned.
			require.NotContains(t, joined, "SCAN ", "move lookup must not scan a table: %s", joined)
			index := "idx_events_moved_from"
			if ref == issue.UID {
				index = "idx_events_issue_uid"
			}
			require.Contains(t, joined, "INDEX "+index, "move lookup must search move history by its old reference: %s", joined)
		})
	}
}

func TestMovedIssueLookupPreservesEveryShortIDLength(t *testing.T) {
	store := openTestDB(t)
	source, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	for length := shortid.MinLength; length <= shortid.MaxLength; length++ {
		t.Run(fmt.Sprintf("length=%d", length), func(t *testing.T) {
			issueUID := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69AAA%02d", length)
			oldShort, err := shortid.Derive(issueUID, length)
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: issueUID, ShortIDOverride: oldShort, Title: "Moved issue", Author: "agent"})
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "agent"})
			require.NoError(t, err)
			found, err := db.MovedIssueUID(t.Context(), store, source.ID, oldShort, false)
			require.NoError(t, err)
			require.Equal(t, issueUID, found)
		})
	}
}
