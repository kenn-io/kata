package dbtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
	"hegel.dev/go/hegel"
)

type commentIdentityTestExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func updateCommentIdentityForTest(ctx context.Context, t *testing.T, store db.Storage, backend Backend, field, value string, commentID int64) {
	t.Helper()
	exec, ok := store.(commentIdentityTestExecutor)
	require.True(t, ok, "storage test adapter must expose SQL for legacy spelling setup")
	query := "UPDATE comments SET " + field + "=? WHERE id=?"
	if backend.Name == "postgres" {
		query = "UPDATE comments SET " + field + "=$1 WHERE id=$2"
	}
	result, err := exec.ExecContext(ctx, query, value, commentID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)
}

func checkSelectedCommentGraphCanonicalIdentity(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	selected, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Selected issue", Author: "worker"})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Other issue", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reader", Body: "Finding"})
	require.NoError(t, err)
	target.UID = "0" + strings.Repeat("A", 24) + "Z"
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", target.UID, target.ID)
	malformed, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reader", Body: "Unrelated opaque identity"})
	require.NoError(t, err)
	malformedUID := strings.Replace(target.UID, "Z", "\u017f", 1)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", malformedUID, malformed.ID)
	reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: selected.ID, Author: "worker", Body: "Response", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	incoming, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reviewer", Body: "Follow-up", ReplyToUID: reply.UID, ReplyKind: "reply"})
	require.NoError(t, err)

	lowerTargetUID := strings.ToLower(target.UID)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", lowerTargetUID, target.ID)
	updateCommentIdentityForTest(ctx, t, store, backend, "reply_to_uid", strings.ToLower(reply.UID), incoming.ID)

	queries := []struct {
		name  string
		query db.CommentGraphQuery
	}{
		{
			name:  "project graph",
			query: db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: selected.ID},
		},
		{
			name: "authorized issue scope",
			query: db.CommentGraphQuery{
				ProjectID: project.ID, SelectedIssueID: selected.ID,
				AllowedIssueIDs: []int64{selected.ID, other.ID},
			},
		},
	}
	for _, test := range queries {
		t.Run(test.name, func(t *testing.T) {
			graph, err := store.ReadCommentGraph(ctx, test.query)
			require.NoError(t, err)
			byUID := make(map[string]db.Comment, len(graph.Comments))
			for _, record := range graph.Comments {
				byUID[record.Comment.UID] = record.Comment
			}
			require.Contains(t, byUID, lowerTargetUID, "case-normalized outgoing target remains in the selected graph")
			require.Contains(t, byUID, reply.UID)
			require.Contains(t, byUID, incoming.UID, "case-normalized incoming reply remains in the selected graph")
			require.NotContains(t, byUID, malformedUID, "malformed Unicode identifiers must not alias a valid ULID")
			require.Equal(t, target.UID, byUID[reply.UID].ReplyToUID)
			require.Equal(t, strings.ToLower(reply.UID), byUID[incoming.UID].ReplyToUID)
		})
	}
	t.Run("shrunk_zero_ulid_lowercase_tail", func(t *testing.T) {
		canonical := strings.Repeat("0", 25) + "A"
		caseVariant := strings.Repeat("0", 25) + "a"
		updateCommentIdentityForTest(t.Context(), t, store, backend, "uid", canonical, target.ID)
		updateCommentIdentityForTest(t.Context(), t, store, backend, "reply_to_uid", caseVariant, reply.ID)

		graph, err := store.ReadCommentGraph(t.Context(), db.CommentGraphQuery{
			ProjectID: project.ID, SelectedIssueID: selected.ID,
		})
		require.NoError(t, err)
		byUID := make(map[string]db.Comment, len(graph.Comments))
		for _, record := range graph.Comments {
			byUID[record.Comment.UID] = record.Comment
		}
		require.Contains(t, byUID, canonical, "the shrunk ULID casing counterexample remains a direct graph neighbor")
		require.Equal(t, caseVariant, byUID[reply.UID].ReplyToUID)
	})

	hegel.Test(t, func(ht *hegel.T) {
		first := hegel.Draw(ht, hegel.SampledFrom([]string{"0", "1", "2", "3", "4", "5", "6", "7"}))
		tail := hegel.Draw(ht, hegel.Text().Alphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ").MinSize(25).MaxSize(25))
		mask := hegel.Draw(ht, hegel.Text().Alphabet("01").MinSize(25).MaxSize(25))
		canonical := first + tail
		alias := []byte(canonical)
		for index, value := range alias[1:] {
			if value >= 'A' && value <= 'Z' && mask[index] == '1' {
				alias[index+1] = value + ('a' - 'A')
			}
		}
		caseVariant := string(alias)
		updateCommentIdentityForTest(t.Context(), t, store, backend, "uid", canonical, target.ID)
		updateCommentIdentityForTest(t.Context(), t, store, backend, "reply_to_uid", caseVariant, reply.ID)

		graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
			ProjectID: project.ID, SelectedIssueID: selected.ID,
		})
		require.NoError(ht, err)
		byUID := make(map[string]db.Comment, len(graph.Comments))
		for _, record := range graph.Comments {
			byUID[record.Comment.UID] = record.Comment
		}
		require.Contains(ht, byUID, canonical, "valid ULID case variants retain direct graph neighbors")
		require.Equal(ht, caseVariant, byUID[reply.UID].ReplyToUID)
	}, hegel.WithTestCases(64), hegel.WithDatabase(t.TempDir()))
	return nil
}

func checkCanonicalDuplicateReplyAfterNormalization(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	targetIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Finding", Author: "finder"})
	require.NoError(t, err)
	firstReplyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "First response", Author: "worker"})
	require.NoError(t, err)
	secondReplyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Second response", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: targetIssue.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	first, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: firstReplyIssue.ID, Author: "worker", Teammate: "reviewer", Body: "First response",
		ReplyToUID: target.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	lowerTargetUID := strings.ToLower(target.UID)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", lowerTargetUID, target.ID)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: secondReplyIssue.ID, Author: "worker", Teammate: "reviewer", Body: "Duplicate response",
		ReplyToUID: lowerTargetUID, ReplyKind: "reply", ValidateReply: true,
	})
	var duplicate *db.DuplicateCommentReplyError
	require.ErrorAs(t, err, &duplicate, "canonical target identity must prevent a duplicate reply after normalization")
	require.Equal(t, first.UID, duplicate.Comment.UID)
	return nil
}

func checkCanonicalCommentIdempotencyAfterNormalization(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	comment, event, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "worker", Body: "Finding",
		IdempotencyKey: "retry-key", IdempotencyFingerprint: "stable-fingerprint",
	})
	require.NoError(t, err)
	lowerCommentUID := strings.ToLower(comment.UID)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", lowerCommentUID, comment.ID)
	var originalPayload struct {
		CommentUID string `json:"comment_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &originalPayload))
	require.Equal(t, comment.UID, originalPayload.CommentUID, "identity lookup must not rewrite the original event payload")

	match, err := store.LookupCommentIdempotency(ctx, issue.UID, "retry-key", time.Time{})
	require.NoError(t, err)
	require.NotNil(t, match)
	require.Equal(t, lowerCommentUID, match.Comment.UID)
	require.Equal(t, "stable-fingerprint", match.Fingerprint)
	return nil
}

func checkCanonicalCommentProvenanceAfterNormalization(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Finding", Author: "actor"})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signedCtx := db.WithRootAttribution(teammate.WithRequest(ctx, "writer"), db.RootAttributionSigner{
		AuthorityUID: pin.AuthorityUID, PrivateKey: private,
	}, "member")
	comment, signedEvent, err := store.CreateComment(signedCtx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "actor", Teammate: "route", Body: "Signed creation",
	})
	require.NoError(t, err)
	lowerCommentUID := strings.ToLower(comment.UID)
	exec, ok := store.(commentIdentityTestExecutor)
	require.True(t, ok, "storage test adapter must expose SQL for legacy spelling setup")
	query := `UPDATE federation_entity_provenance SET entity_uid=? WHERE project_uid=? AND kind='comment' AND entity_uid=?`
	if backend.Name == "postgres" {
		query = `UPDATE federation_entity_provenance SET entity_uid=$1 WHERE project_uid=$2 AND kind='comment' AND entity_uid=$3`
	}
	result, err := exec.ExecContext(ctx, query, lowerCommentUID, project.UID, comment.UID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "verified", comments[0].Verification,
		"a canonicalized comment row must retain its signed attribution projection")
	receipt, err := store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
	require.NoError(t, err)
	require.Equal(t, signedEvent.UID, receipt.EventUID)
	require.Equal(t, "route", receipt.Teammate)

	exportedEntity := false
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		ref, ok := record.(*db.EntityProvenance)
		if ok && ref.Kind == "comment" && ref.EntityUID == lowerCommentUID {
			exportedEntity = true
		}
	}
	require.True(t, exportedEntity, "project export retains the original provenance reference")
	return nil
}
