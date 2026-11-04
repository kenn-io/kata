package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
	"modernc.org/sqlite"
)

func TestCommentDisplayContributionsUsesEventIndexes(t *testing.T) {
	store := openTestDB(t)
	ctx := t.Context()
	historyProject := createProject(ctx, t, store, "history-project")
	project := createProject(ctx, t, store, "current-project")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: historyProject.ID,
		Title:     "Target issue",
		Author:    "writer",
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "writer",
		Body:    "Current comment",
	})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, "UPDATE events SET issue_uid=NULL WHERE issue_uid=?", issue.UID)
	require.NoError(t, err)

	snapshotCommentUID, err := uid.New()
	require.NoError(t, err)
	snapshotEventUID, err := uid.New()
	require.NoError(t, err)
	emptyEnvelopeCommentUID, err := uid.New()
	require.NoError(t, err)
	emptyEnvelopeEventUID, err := uid.New()
	require.NoError(t, err)
	originUID, err := uid.New()
	require.NoError(t, err)
	snapshotPayload := fmt.Sprintf(
		`{"uid":%q,"comments":[{"comment_uid":%q,"author":"writer","teammate":"route","attributed_teammate":"reviewer","body":"Snapshot comment","created_at":"2026-11-03T12:00:00Z"}]}`,
		issue.UID, snapshotCommentUID,
	)
	_, err = store.ExecContext(ctx, `INSERT INTO events(
		uid, origin_instance_uid, project_id, project_name, type, actor, payload,
		hlc_physical_ms, hlc_counter, content_hash
	) VALUES(?, ?, ?, ?, 'issue.snapshot', 'writer', ?, 1, 0, ?)`,
		snapshotEventUID, originUID, historyProject.ID, historyProject.Name, snapshotPayload, strings.Repeat("a", 64))
	require.NoError(t, err)
	emptyEnvelopePayload := fmt.Sprintf(
		`{"uid":%q,"comments":[{"comment_uid":%q,"author":"writer","teammate":"route","attributed_teammate":"editor","body":"Empty-envelope snapshot comment","created_at":"2026-11-03T12:01:00Z"}]}`,
		issue.UID, emptyEnvelopeCommentUID,
	)
	_, err = store.ExecContext(ctx, `INSERT INTO events(
		uid, origin_instance_uid, project_id, project_name, issue_uid, type, actor,
		payload, hlc_physical_ms, hlc_counter, content_hash
	) VALUES(?, ?, ?, ?, '', 'issue.snapshot', 'writer', ?, 2, 0, ?)`,
		emptyEnvelopeEventUID, originUID, historyProject.ID, historyProject.Name, emptyEnvelopePayload, strings.Repeat("c", 64))
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: historyProject.ID, ToProjectID: project.ID,
		IfMatchRev: issue.Revision, Actor: "writer",
	})
	require.NoError(t, err)

	otherProject := createProject(ctx, t, store, "other-project")
	for i := range 256 {
		eventUID, err := uid.New()
		require.NoError(t, err)
		_, err = store.ExecContext(ctx, `INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name, type, actor, payload,
			hlc_physical_ms, hlc_counter, content_hash
		) VALUES(?, ?, ?, ?, 'issue.updated', 'writer', '{}', ?, 0, ?)`,
			eventUID, originUID, otherProject.ID, otherProject.Name, i+1, strings.Repeat("b", 64))
		require.NoError(t, err)
	}
	_, err = store.ExecContext(ctx, "ANALYZE events")
	require.NoError(t, err)

	type capturedQuery struct {
		statement string
		args      []any
	}
	var captured []capturedQuery
	query := func(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
		captured = append(captured, capturedQuery{statement: statement, args: append([]any(nil), args...)})
		return store.QueryContext(ctx, statement, args...)
	}
	display, err := db.CommentDisplayContributions(ctx, query, issue.ID, func(int) string { return "?" }, db.CommentDisplaySQLite)
	require.NoError(t, err)
	require.Equal(t, "reviewer", display[snapshotCommentUID], "payload-only snapshot history must remain visible")
	require.Equal(t, "editor", display[emptyEnvelopeCommentUID], "empty-envelope snapshot history must remain visible")

	var plan []string
	for _, call := range captured {
		rows, err := store.QueryContext(ctx, "EXPLAIN QUERY PLAN "+call.statement, call.args...)
		require.NoError(t, err)
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "idx_events_issue_uid", "normal issue history must use its UID index: %s", joined)
	require.Contains(t, joined, "idx_events_project", "legacy envelope history must be scoped by project: %s", joined)
	require.NotContains(t, joined, "SCAN e", "comment display history must not scan all events: %s", joined)
	require.NotContains(t, joined, "SCAN moved", "move history must be looked up by issue UID: %s", joined)
}

func TestCreateCommentUsesItsCreationEventWithoutReadingThreadHistory(t *testing.T) {
	ctx := t.Context()
	tempDir := t.TempDir()
	databasePath := filepath.Join(tempDir, "kata.db")
	dsnMarker := filepath.Base(tempDir)
	var eventPayloadReads atomic.Int64
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		if !strings.Contains(dsn, dsnMarker) {
			return nil
		}
		registerer, ok := conn.(sqlite.AuthorizerRegisterer)
		if !ok {
			return fmt.Errorf("SQLite connection %T does not support authorizers", conn)
		}
		return registerer.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, table, column, _, _ string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthRead && table == "events" && column == "payload" {
				eventPayloadReads.Add(1)
			}
			return sqlite.AuthorizerOK
		})
	})
	store, err := sqlitestore.Open(ctx, databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project := createProject(ctx, t, store, "comment-create-project")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "New comment attribution", Author: "writer",
	})
	require.NoError(t, err)

	eventPayloadReads.Store(0)

	comment, event, err := store.CreateComment(teammate.WithRequest(ctx, "writer"), db.CreateCommentParams{
		IssueID: issue.ID, Author: "writer", Teammate: "route", Body: "New comment",
	})
	require.NoError(t, err)
	require.LessOrEqual(t, eventPayloadReads.Load(), int64(1), "comment creation must not replay thread history")
	require.NotNil(t, comment.AttributedTeammate)
	require.Equal(t, db.DisplayContribution(event.Type, []byte(event.Payload)), *comment.AttributedTeammate)
}
