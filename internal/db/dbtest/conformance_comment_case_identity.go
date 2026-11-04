package dbtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
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

func drawCommentIdentityULID(ht *hegel.T) string {
	first := hegel.Draw(ht, hegel.SampledFrom([]string{"0", "1", "2", "3", "4", "5", "6", "7"}))
	tail := []byte(hegel.Draw(ht, hegel.Text().Alphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ").MinSize(24).MaxSize(24)))
	last := hegel.Draw(ht, hegel.SampledFrom([]string{"A", "B", "C", "D", "E", "F", "G", "H", "J", "K", "M", "N", "P", "Q", "R", "S", "T", "V", "W", "X", "Y", "Z"}))
	return first + string(tail) + last
}

func drawCommentIdentityCaseVariant(ht *hegel.T, identity string) string {
	variant := []byte(identity)
	for index := 1; index < len(variant)-1; index++ {
		if variant[index] >= 'A' && variant[index] <= 'Z' && hegel.Draw(ht, hegel.Booleans()) {
			variant[index] += 'a' - 'A'
		}
	}
	variant[len(variant)-1] += 'a' - 'A'
	return string(variant)
}

func checkProjectMergeRejectsCanonicalCommentUIDCollision(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	targetProject, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	targetIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: targetProject.ID, Title: "Existing issue", Author: "worker",
	})
	require.NoError(t, err)
	existingComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: targetIssue.ID, Author: "worker", Body: "Existing comment",
	})
	require.NoError(t, err)

	const importedAt = "2026-11-04T00:00:00.000Z"
	stableTime := time.Date(2026, time.November, 4, 0, 0, 0, 0, time.UTC)
	var trial uint64

	hegel.Test(t, func(ht *hegel.T) {
		trial++
		trialSeed := strconv.FormatUint(trial, 10)
		commentUID, err := uid.FromStableSeed([]byte("comment:"+trialSeed), stableTime)
		require.NoError(ht, err)
		last := hegel.Draw(ht, hegel.SampledFrom([]string{
			"A", "B", "C", "D", "E", "F", "G", "H", "J", "K", "M", "N", "P", "Q", "R", "S", "T", "V", "W", "X", "Y", "Z",
		}))
		commentUID = commentUID[:len(commentUID)-1] + last
		storedVariant := commentUID[:len(commentUID)-1] + strings.ToLower(last)
		updateCommentIdentityForTest(ctx, t, store, backend, "uid", storedVariant, existingComment.ID)
		importedProjectUID, err := uid.FromStableSeed([]byte("project:"+trialSeed), stableTime)
		require.NoError(ht, err)
		importedIssueUID, err := uid.FromStableSeed([]byte("issue:"+trialSeed), stableTime)
		require.NoError(ht, err)

		records := []db.ImportRecord{
			&db.ProjectExport{
				ID: 1, UID: importedProjectUID, Name: "spoke-project-" + trialSeed, CreatedAt: importedAt,
				Metadata: []byte(`{}`), Revision: 1,
			},
			&db.IssueExport{
				ID: 1, UID: importedIssueUID, ProjectID: 1,
				ShortID: strings.ToLower(importedIssueUID[len(importedIssueUID)-4:]), Title: "Imported issue",
				Status: "open", Author: "worker", CreatedAt: importedAt, UpdatedAt: importedAt,
				Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
			},
			&db.CommentExport{
				ID: 1, UID: commentUID, IssueID: 1, Author: "worker", Body: "Imported comment",
				CreatedAt: importedAt,
			},
		}
		err = store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true})
		require.ErrorContains(ht, err, "comment UID")
		_, err = store.ProjectByUID(ctx, importedProjectUID)
		require.ErrorIs(ht, err, db.ErrNotFound, "a UID collision must roll back the whole merge")
	}, hegel.WithTestCases(1), hegel.WithDatabase(t.TempDir()))
	return nil
}

func checkCommentIssueIDsByCanonicalUID(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	firstIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "First issue", Author: "worker",
	})
	require.NoError(t, err)
	secondIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Second issue", Author: "worker",
	})
	require.NoError(t, err)
	firstComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: firstIssue.ID, Author: "worker", Body: "First comment",
	})
	require.NoError(t, err)
	secondComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: secondIssue.ID, Author: "worker", Body: "Second comment",
	})
	require.NoError(t, err)

	hegel.Test(t, func(ht *hegel.T) {
		canonical := drawCommentIdentityULID(ht)
		storedVariant := drawCommentIdentityCaseVariant(ht, canonical)
		firstAlternate := byte('0')
		if canonical[0] == firstAlternate {
			firstAlternate = '1'
		}
		secondAlternate := byte('2')
		if canonical[0] == secondAlternate || firstAlternate == secondAlternate {
			secondAlternate = '3'
		}
		collisionUID := string(firstAlternate) + canonical[1:]
		missingUID := string(secondAlternate) + canonical[1:]
		updateCommentIdentityForTest(ctx, t, store, backend, "uid", storedVariant, firstComment.ID)
		updateCommentIdentityForTest(ctx, t, store, backend, "uid", collisionUID, secondComment.ID)

		requestedVariant := drawCommentIdentityCaseVariant(ht, canonical)
		requestedCollision := drawCommentIdentityCaseVariant(ht, collisionUID)
		requestedMissing := drawCommentIdentityCaseVariant(ht, missingUID)
		issueIDs, err := store.CommentIssueIDsByUIDs(ctx, []string{
			requestedVariant, requestedCollision, requestedMissing,
		})
		require.NoError(ht, err)
		require.Equal(ht, firstIssue.ID, issueIDs[requestedVariant])
		require.Equal(ht, secondIssue.ID, issueIDs[requestedCollision])
		require.NotContains(ht, issueIDs, requestedMissing,
			"a same-handle candidate must still match the complete canonical UID")
	}, hegel.WithTestCases(64), hegel.WithDatabase(t.TempDir()))
	return nil
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
	target.UID = "0" + strings.Repeat("A", 24) + "S"
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", target.UID, target.ID)
	malformed, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reader", Body: "Unrelated opaque identity"})
	require.NoError(t, err)
	malformedUID := strings.Replace(target.UID, "S", "\u017f", 1)
	require.Equal(t, target.UID, strings.ToUpper(malformedUID),
		"the malformed Unicode spelling must uppercase to the valid ULID target")
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

func checkCaseVariantBacklinkHandleCollision(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	selected, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Selected issue", Author: "worker"})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Other issue", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: selected.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	incoming, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: other.ID, Author: "reviewer", Body: "Reply", ReplyToUID: target.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	collision, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: other.ID, Author: "worker", Body: "Unrelated comment with a colliding suffix",
	})
	require.NoError(t, err)

	targetUID := strings.Repeat("0", 20) + "BBBBBS"
	incomingUID := strings.Repeat("1", 20) + "AAAAAA"
	collisionUID := strings.Repeat("2", 20) + "AAAAAA"
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", targetUID, target.ID)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", incomingUID, incoming.ID)
	updateCommentIdentityForTest(ctx, t, store, backend, "uid", collisionUID, collision.ID)
	updateCommentIdentityForTest(ctx, t, store, backend, "reply_to_uid", strings.ToLower(targetUID), incoming.ID)

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: project.ID, SelectedIssueID: selected.ID,
	})
	require.NoError(t, err)
	handles := commentref.Handles(graph.CommentUIDsByProject[project.ID])
	require.Contains(t, graph.CommentUIDsByProject[project.ID], incomingUID,
		"the case-variant incoming reply contributes its suffix to project handle discovery")
	require.Contains(t, graph.CommentUIDsByProject[project.ID], collisionUID,
		"the unrelated colliding suffix must extend the advertised backlink")

	records := make([]commentref.Record, 0, len(graph.Comments))
	for _, row := range graph.Comments {
		record := commentref.Record{
			Comment: row.Comment, IssueUID: row.IssueUID, IssueShortID: row.IssueShortID,
			ProjectUID: row.ProjectUID, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
		}
		if suffix := handles[record.UID]; suffix != "" {
			record.Handle = "c:" + suffix
		}
		records = append(records, record)
	}
	projected := commentref.Project(records, nil)
	var backlink commentref.Link
	for _, record := range projected {
		if record.UID == targetUID {
			require.Len(t, record.Backlinks, 1)
			backlink = record.Backlinks[0]
		}
	}
	require.NotEmpty(t, backlink.Handle)
	ref, err := commentref.Parse(backlink.Handle)
	require.NoError(t, err)
	require.Greater(t, len(ref.Suffix), commentref.MinLength,
		"the backlink suffix must exclude the unrelated comment sharing its first six characters")

	lookup, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: project.ID,
		Reference: &db.CommentGraphReference{Suffix: ref.Suffix, IssueShortID: other.ShortID},
	})
	require.NoError(t, err)
	lookupRecords := make([]commentref.Record, 0, len(lookup.Comments))
	for _, row := range lookup.Comments {
		lookupRecords = append(lookupRecords, commentref.Record{
			Comment: row.Comment, IssueUID: row.IssueUID, IssueShortID: row.IssueShortID,
			ProjectUID: row.ProjectUID, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
		})
	}
	resolved, err := commentref.Resolve(lookupRecords, selected.UID, backlink.Handle, project.Name)
	require.NoError(t, err, "the advertised backlink must resolve without an ambiguous_comment error")
	require.Equal(t, incomingUID, resolved.UID)
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

func checkCommentEditCanonicalIdentity(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)

	updated, event, changed, err := store.EditComment(ctx, db.EditCommentParams{
		IssueID: issue.ID, CommentUID: strings.ToLower(comment.UID), Actor: "worker", Body: "Revised finding",
	})
	require.NoError(t, err, "a case variant of a valid comment ULID must select the same identity")
	require.True(t, changed)
	require.Equal(t, comment.UID, updated.UID)
	require.NotNil(t, event)
	var payload struct {
		CommentUID string `json:"comment_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	require.Equal(t, comment.UID, payload.CommentUID)
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
