package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/uid"
)

func TestCommentNotificationTransaction(t *testing.T) {
	store, e := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = store.Close() })
	p, e := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, e)
	parent, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Parent", Author: "lead", Owner: new("lead")})
	require.NoError(t, e)
	issue, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker", Owner: new("owner")})
	require.NoError(t, e)
	_, e = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: parent.ID, Type: "parent", Author: "lead"})
	require.NoError(t, e)
	target, _, e := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Teammate: "review", Body: "Finding"})
	require.NoError(t, e)
	for i := range 10 {
		_, _, e = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: string(rune('a' + i)), Body: "Prior", ReplyToUID: target.UID, ReplyKind: "reply"})
		require.NoError(t, e)
	}
	var events []db.Event
	reply, _, e := store.CreateComment(db.WithCommentMetadataHook(t.Context(), commentNotificationHook(), &events), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Teammate: "builder", Body: "Confirmed with detailed reproduction evidence", ReplyToUID: target.UID, ReplyKind: "confirm"})
	require.NoError(t, e)
	current, e := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, e)
	var slots map[string]jsontext.Value
	slots = nil
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.Len(t, slots, 11)
	require.Contains(t, slots, notification.MetadataKey("reader/review"))
	require.Contains(t, slots, notification.MetadataKey("lead"))
	require.NotContains(t, slots, notification.MetadataKey("a"))
	require.Contains(t, slots, notification.MetadataKey("c"))
	var value notification.Value
	require.NoError(t, json.Unmarshal(slots[notification.MetadataKey("reader/review")], &value))
	require.Equal(t, reply.UID, value.Re)
	require.Len(t, events, 2)
	// Another confirmation must skip every recipient already notified about target.
	events = nil
	_, _, e = store.CreateComment(db.WithCommentMetadataHook(t.Context(), commentNotificationHook(), &events), db.CreateCommentParams{IssueID: issue.ID, Author: "other", Body: "Second independent reproduction with evidence", ReplyToUID: target.UID, ReplyKind: "confirm"})
	require.NoError(t, e)
	current, e = store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, e)
	slots = nil
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.NoError(t, json.Unmarshal(slots[notification.MetadataKey("reader/review")], &value))
	require.Equal(t, reply.UID, value.Re)
	// Matching reply clears exact recipient slot and notifies original reply author.
	_, _, e = store.CreateComment(db.WithCommentMetadataHook(t.Context(), commentNotificationHook(), &events), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Teammate: "review", Body: "Answer", ReplyToUID: reply.UID, ReplyKind: "reply"})
	require.NoError(t, e)
	current, e = store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, e)
	slots = nil
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.NotContains(t, slots, notification.MetadataKey("reader/review"))
	require.Contains(t, slots, notification.MetadataKey("worker/builder"))
}

func FuzzNotificationProjectionWhitespace(f *testing.F) {
	f.Add(" ", "secret")
	f.Fuzz(func(t *testing.T, space, message string) {
		if len(space)+len(message) > 2048 {
			return
		}
		for _, r := range space {
			if r != ' ' && r != '\n' && r != '\t' && r != '\r' {
				return
			}
		}
		slot, e := json.Marshal(map[string]any{"from": "lead", "message": message, "re": "hidden"})
		if e != nil {
			return
		}
		raw := jsontext.Value(space + `{"metadata":{"notify.reader":` + string(slot) + `},"other":1}`)
		got, e := walkNotificationJSON(raw, func(value jsontext.Value) (bool, error) { return len(notificationReferences(value)) == 0, nil })
		require.NoError(t, e)
		require.JSONEq(t, `{"metadata":{},"other":1}`, string(got))
	})
}

func TestCommentNotificationsScopedDiscovery(t *testing.T) {
	store, e := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = store.Close() })
	p, e := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, e)
	parent, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Outside parent", Author: "lead", Owner: new("private-owner")})
	require.NoError(t, e)
	source, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Visible root", Author: "worker"})
	require.NoError(t, e)
	hidden, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Outside issue", Author: "worker"})
	require.NoError(t, e)
	_, e = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: parent.ID, Type: "parent", Author: "lead"})
	require.NoError(t, e)
	target, _, e := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: source.ID, Author: "reader", Body: "Finding"})
	require.NoError(t, e)
	_, _, e = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: hidden.ID, Author: "private-linker", Body: "Earlier", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, e)
	ctx := WithPrincipal(t.Context(), Principal{Kind: PrincipalDBToken, Actor: "worker", Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: source.UID}})
	_, _, e = store.CreateComment(db.WithCommentMetadataHook(ctx, commentNotificationHook(), nil), db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Confirmation", ReplyToUID: target.UID, ReplyKind: "confirm"})
	require.NoError(t, e)
	current, e := store.IssueByID(t.Context(), source.ID)
	require.NoError(t, e)
	var slots map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.Contains(t, slots, notification.MetadataKey("reader"))
	require.NotContains(t, slots, notification.MetadataKey("private-owner"))
	require.NotContains(t, slots, notification.MetadataKey("private-linker"))
}

func TestNotificationProjectionLeadingWhitespace(t *testing.T) {
	raw := jsontext.Value(" \n" + `{"metadata":{"notify.reader":{"re":"hidden","message":"secret"}}}`)
	got, err := walkNotificationJSON(raw, func(slot jsontext.Value) (bool, error) { return len(notificationReferences(slot)) == 0, nil })
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{}}`, string(got))
}

func TestBroadcastHistoryIncludesFractionalBoundary(t *testing.T) {
	checkBroadcastHistoryBoundary(t, 0)
}

func TestBroadcastHistoryIncludesFractionalBoundaryPostgres(t *testing.T) {
	checkBroadcastHistoryBoundaryBackend(t, 0, true)
}
func FuzzBroadcastHistoryBoundary(f *testing.F) {
	f.Add(uint8(0))
	f.Fuzz(func(t *testing.T, second uint8) { checkBroadcastHistoryBoundary(t, second%60) })
}

// The last-hour query must include an event later in the cutoff second. Event
// timestamps use the stores' canonical millisecond representation.
func checkBroadcastHistoryBoundary(t *testing.T, second uint8) {
	t.Helper()
	checkBroadcastHistoryBoundaryBackend(t, second, false)
}

func checkBroadcastHistoryBoundaryBackend(t *testing.T, second uint8, postgres bool) {
	t.Helper()
	var store interface {
		db.Storage
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}
	if postgres {
		dsn := os.Getenv("KATA_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires explicit PostgreSQL test service")
		}
		id, err := uid.New()
		require.NoError(t, err)
		schema := "notify_boundary_" + strings.ToLower(id)
		opened, err := pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
		store = opened
		t.Cleanup(func() {
			admin, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			defer func() { _ = admin.Close() }()
			_, err = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			require.NoError(t, err)
		})
	} else {
		opened, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "example.db"))
		require.NoError(t, err)
		store = opened
	}
	t.Cleanup(func() { _ = store.Close() })
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Broadcast", Author: "sender"})
	require.NoError(t, err)
	value, err := json.Marshal(notification.Value{From: "sender", Message: "Inspect finding", Broadcast: true})
	require.NoError(t, err)
	out, err := store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "sender", Patch: map[string]jsontext.Value{notification.MetadataKey("reader"): value}})
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, int(second), 0, time.UTC)
	at := now.Add(-time.Hour + 500*time.Millisecond)
	_, err = store.ExecContext(t.Context(), `UPDATE events SET created_at=$1 WHERE id=$2`, at.Format("2006-01-02T15:04:05.000Z"), out.Event.ID)
	require.NoError(t, err)
	tx, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	records, err := broadcastHistoryTx(t.Context(), tx, issue.ID, store.InstanceUID(), now)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, at, records[0].At)
}

func TestCommentNotificationUsesUniqueHandle(t *testing.T) {
	checkUniqueNotificationHandle(t, 6)
}

func FuzzCommentNotificationUniqueHandle(f *testing.F) {
	f.Add(uint8(6))
	f.Fuzz(func(t *testing.T, depth uint8) { checkUniqueNotificationHandle(t, 6+int(depth%5)) })
}

func checkUniqueNotificationHandle(t *testing.T, depth int) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "example.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Finding", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Body: "Finding"})
	require.NoError(t, err)
	reply, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Answer", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	replyUID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	targetUID := []byte(replyUID)
	position := len(targetUID) - depth - 1
	if targetUID[position] == '0' {
		targetUID[position] = '1'
	} else {
		targetUID[position] = '0'
	}
	_, err = store.ExecContext(t.Context(), `UPDATE comments SET uid=$1 WHERE id=$2`, string(targetUID), target.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `UPDATE comments SET uid=$1,reply_to_uid=$2 WHERE id=$3`, replyUID, string(targetUID), reply.ID)
	require.NoError(t, err)
	reply.UID, reply.ReplyToUID = replyUID, string(targetUID)
	tx, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	patch, err := commentNotificationHook()(t.Context(), tx, issue, reply)
	require.NoError(t, err)
	var value notification.Value
	require.NoError(t, json.Unmarshal(patch[notification.MetadataKey("reader")], &value))
	require.Equal(t, "latest: reply c:"+strings.ToLower(replyUID[len(replyUID)-depth-1:])+" by worker", value.Message)
}
