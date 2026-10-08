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
	"unicode/utf8"

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

func TestCommentNotificationConfirmUsesNewestTimestampInstant(t *testing.T) {
	checkCommentNotificationConfirmTimestampInstantOrder(t, "12", "1")
}

func FuzzCommentNotificationConfirmTimestampInstantOrder(f *testing.F) {
	f.Add("12", "1")
	f.Fuzz(func(t *testing.T, newerFraction, olderFraction string) {
		if !fractionalTimestampDigits(newerFraction) || !fractionalTimestampDigits(olderFraction) {
			return
		}
		newerText := "2026-10-08T12:00:00." + newerFraction + "Z"
		olderText := "2026-10-08T12:00:00." + olderFraction + "Z"
		newer, err := time.Parse(time.RFC3339Nano, newerText)
		if err != nil {
			return
		}
		older, err := time.Parse(time.RFC3339Nano, olderText)
		if err != nil || !newer.After(older) || newerText >= olderText {
			return
		}
		checkCommentNotificationConfirmTimestampInstantOrder(t, newerFraction, olderFraction)
	})
}

func fractionalTimestampDigits(value string) bool {
	if value == "" || len(value) > 9 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func checkCommentNotificationConfirmTimestampInstantOrder(t *testing.T, newerFraction, olderFraction string) {
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

	prior := make([]db.Comment, 0, 9)
	for _, actor := range []string{"newer-fraction", "older-fraction", "later-a", "later-b", "later-c", "later-d", "later-e", "later-f", "later-g"} {
		comment, _, createErr := store.CreateComment(t.Context(), db.CreateCommentParams{
			IssueID: issue.ID, Author: actor, Body: "Earlier reply", ReplyToUID: target.UID, ReplyKind: "reply",
		})
		require.NoError(t, createErr)
		prior = append(prior, comment)
	}

	timestamps := []string{
		"2026-10-08T12:00:00." + newerFraction + "Z",
		"2026-10-08T12:00:00." + olderFraction + "Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
		"2026-10-08T12:00:01.000Z",
	}
	for i, comment := range prior {
		_, err := store.ExecContext(t.Context(), `UPDATE comments SET created_at=$1 WHERE uid=$2`, timestamps[i], comment.UID)
		require.NoError(t, err)
	}

	_, _, err = store.CreateComment(
		db.WithCommentMetadataHook(t.Context(), commentNotificationHook(), nil),
		db.CreateCommentParams{IssueID: issue.ID, Author: "confirmer", Body: "Confirmed with reproduction evidence", ReplyToUID: target.UID, ReplyKind: "confirm"},
	)
	require.NoError(t, err)
	updated, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	var metadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(updated.Metadata), &metadata))
	require.Contains(t, metadata, notification.MetadataKey("newer-fraction"), "the newest instant must remain inside the eight-linker fan-out")
	require.NotContains(t, metadata, notification.MetadataKey("older-fraction"), "the older instant must fall outside the eight-linker fan-out")
}

func TestCommentNotificationCrossIssueReplyClearsRequest(t *testing.T) {
	checkCrossIssueReplyAutoClear(t, 2, 0b01)
}

func FuzzCommentNotificationCrossIssueReplyAutoClear(f *testing.F) {
	f.Add(uint8(2), uint8(0b01))
	f.Add(uint8(4), uint8(0b1010))
	f.Add(uint8(1), uint8(0))
	f.Fuzz(func(t *testing.T, requestCount, matchingMask uint8) {
		count := int(requestCount%5) + 1
		mask := matchingMask & uint8((1<<count)-1)
		checkCrossIssueReplyAutoClear(t, count, mask)
	})
}

func checkCrossIssueReplyAutoClear(t *testing.T, requestCount int, matchingMask uint8) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	notificationIssues := make([]db.Issue, requestCount)
	for index := range notificationIssues {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Notification context", Author: "lead"})
		require.NoError(t, err)
		notificationIssues[index] = issue
	}
	replyIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Comment thread", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: replyIssue.ID, Author: "reader", Body: "Finding"})
	require.NoError(t, err)
	otherTarget, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: replyIssue.ID, Author: "reader", Body: "Another finding"})
	require.NoError(t, err)
	requestKey := notification.MetadataKey("worker")
	matchingIssues := make([]db.Issue, 0, requestCount)
	for index, issue := range notificationIssues {
		re := otherTarget.UID
		if matchingMask&(1<<index) != 0 {
			re = target.UID
			matchingIssues = append(matchingIssues, issue)
		}
		request, err := json.Marshal(notification.Value{From: "lead", Message: "Please respond", Re: re})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
			IssueID: issue.ID,
			Actor:   "lead",
			Patch:   map[string]jsontext.Value{requestKey: request},
		})
		require.NoError(t, err)
	}

	var events []db.Event
	reply, _, err := store.CreateComment(
		db.WithCommentMetadataHook(t.Context(), commentNotificationHook(), &events),
		db.CreateCommentParams{IssueID: replyIssue.ID, Author: "worker", Body: "Answer", ReplyToUID: target.UID, ReplyKind: "reply"},
	)
	require.NoError(t, err)

	for index, issue := range notificationIssues {
		current, err := store.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		var slots map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
		if matchingMask&(1<<index) != 0 {
			require.NotContains(t, slots, requestKey, "a reply must clear the matching notify slot even when its metadata is on another issue")
		} else {
			require.Contains(t, slots, requestKey, "a reply must preserve the recipient slot when it references a different comment")
		}
	}

	currentReplyIssue, err := store.IssueByID(t.Context(), replyIssue.ID)
	require.NoError(t, err)
	var replySlots map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(currentReplyIssue.Metadata), &replySlots))
	var linked notification.Value
	require.NoError(t, json.Unmarshal(replySlots[notification.MetadataKey("reader")], &linked))
	require.Equal(t, "worker", linked.From)
	require.Equal(t, reply.UID, linked.Re)
	require.Equal(t, "reply", linked.Kind)

	require.Len(t, events, 1+len(matchingIssues)+1, "retain the comment event and each coalesced issue metadata event")
	require.Equal(t, "issue.commented", events[0].Type)
	require.Equal(t, replyIssue.ID, *events[0].IssueID)
	require.Equal(t, "worker", events[0].Actor)
	for index, issue := range matchingIssues {
		event := events[index+1]
		require.Equal(t, "issue.metadata_updated", event.Type)
		require.Equal(t, issue.ID, *event.IssueID)
		require.Equal(t, "worker", event.Actor)
	}
	last := events[len(events)-1]
	require.Equal(t, "issue.metadata_updated", last.Type)
	require.Equal(t, replyIssue.ID, *last.IssueID)
	require.Equal(t, "worker", last.Actor)
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

func TestNotificationProjectionPreservesOpaqueNestedValues(t *testing.T) {
	checkNotificationProjectionPreservesOpaqueNestedValues(t, "8")
	checkNotificationProjectionPreservesOpaqueNestedValues(t, "opaque metadata")
}

func FuzzNotificationProjectionPreservesOpaqueNestedValues(f *testing.F) {
	f.Add("8")
	f.Add("opaque metadata")
	f.Add(`JSON-looking text: {"re":"hidden-comment"}`)
	f.Fuzz(func(t *testing.T, message string) {
		if len(message) > 1024 || !utf8.ValidString(message) {
			return
		}
		checkNotificationProjectionPreservesOpaqueNestedValues(t, message)
	})
}

func checkNotificationProjectionPreservesOpaqueNestedValues(t *testing.T, message string) {
	t.Helper()
	slot := map[string]any{"re": "hidden-comment", "message": message}
	opaque := map[string]any{"notify.reader": slot}
	eventPayload, err := json.Marshal(map[string]any{"diff": map[string]any{
		"custom":        opaque,
		"notify.reader": map[string]any{"from": nil, "to": slot},
	}})
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"custom":        opaque,
			"notify.reader": slot,
		},
		"event": map[string]any{
			"type":    "issue.metadata_updated",
			"payload": string(eventPayload),
		},
	})
	require.NoError(t, err)

	got, err := walkNotificationJSON(jsontext.Value(raw), func(value jsontext.Value) (bool, error) {
		for _, uid := range notificationReferences(value) {
			if uid == "hidden-comment" {
				return false, nil
			}
		}
		return true, nil
	})
	require.NoError(t, err)

	var projected struct {
		Metadata jsontext.Value `json:"metadata"`
		Event    struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(got, &projected))
	expectedMetadata, err := json.Marshal(map[string]any{"custom": opaque})
	require.NoError(t, err)
	require.JSONEq(t, string(expectedMetadata), string(projected.Metadata))
	require.Equal(t, "issue.metadata_updated", projected.Event.Type)
	expectedEventPayload, err := json.Marshal(map[string]any{"diff": map[string]any{"custom": opaque}})
	require.NoError(t, err)
	require.JSONEq(t, string(expectedEventPayload), projected.Event.Payload)
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
	updates, err := commentNotificationHook()(t.Context(), tx, issue, reply)
	require.NoError(t, err)
	var patch map[string]jsontext.Value
	for _, update := range updates {
		if update.IssueID == issue.ID {
			patch = update.Patch
		}
	}
	var value notification.Value
	require.NoError(t, json.Unmarshal(patch[notification.MetadataKey("reader")], &value))
	require.Equal(t, "latest: reply c:"+strings.ToLower(replyUID[len(replyUID)-depth-1:])+" by worker", value.Message)
}
