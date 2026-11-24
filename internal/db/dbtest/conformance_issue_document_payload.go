package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// issueDocumentRequiredFields are the payload fields every issue.created and
// issue.snapshot writer has sent since the first federation release.
var issueDocumentRequiredFields = []string{"uid", "short_id", "title", "body", "author", "status", "metadata", "created_at"}

// issueDocumentFields returns a complete issue.created or issue.snapshot
// payload as a field map a test can extend or trim before marshaling.
func issueDocumentFields(issueUID, actor string) map[string]any {
	return map[string]any{
		"uid": issueUID, "short_id": strings.ToLower(issueUID[len(issueUID)-4:]),
		"title": "Creation", "body": "", "author": actor, "status": "open",
		"metadata": map[string]any{}, "created_at": "2026-05-23T12:00:00.000Z",
	}
}

func marshalIssueDocument(t *testing.T, fields map[string]any) jsontext.Value {
	t.Helper()
	payload, err := json.Marshal(fields)
	require.NoError(t, err)
	return payload
}

// incompleteIssueDocument is an issue.created or issue.snapshot of a new
// issue whose payload lacks one required field.
type incompleteIssueDocument struct {
	name    string
	missing string
	event   db.RemoteEvent
}

// rejectsMissing asserts err is the document rule's rejection of this event:
// a validation error that names the missing field.
func (d incompleteIssueDocument) rejectsMissing(t *testing.T, err error, path string) {
	t.Helper()
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "%s must reject %s", path, d.name)
	require.ErrorContains(t, err, "has no "+d.missing, "%s must reject %s by the document rule", path, d.name)
}

// incompleteIssueDocuments returns, for each required field, an issue.created
// and an issue.snapshot of a new issue whose payload lacks that field.
func incompleteIssueDocuments(t *testing.T, project db.Project, actor, origin string) []incompleteIssueDocument {
	t.Helper()
	var events []incompleteIssueDocument
	for _, kind := range []string{"issue.created", "issue.snapshot"} {
		for _, field := range issueDocumentRequiredFields {
			issueUID, err := uid.New()
			require.NoError(t, err)
			fields := issueDocumentFields(issueUID, actor)
			delete(fields, field)
			events = append(events, incompleteIssueDocument{
				name: kind + " without " + field, missing: field,
				event: newRemoteEvent(t, project, &issueUID, kind, actor, origin, 700, marshalIssueDocument(t, fields)),
			})
		}
	}
	return events
}

// checkIssueDocumentsRequireEveryField proves that pushed and pulled issue
// creations and snapshots must carry every field their writers always send,
// that a rejected event is not stored, and that a complete document is
// accepted.
func checkIssueDocumentsRequireEveryField(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	push := func(sourceEventID int64, event db.RemoteEvent) error {
		_, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker",
			Events: []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: event}},
		})
		return err
	}

	for _, doc := range incompleteIssueDocuments(t, project, "worker", origin) {
		doc.rejectsMissing(t, push(1, doc.event), "push")
		_, err := store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{doc.event})
		doc.rejectsMissing(t, err, "pull")
		_, err = store.EventsByUIDs(ctx, project.ID, []string{doc.event.EventUID})
		require.ErrorIs(t, err, db.ErrNotFound, "a rejected %s must not be stored", doc.name)
	}

	for i, kind := range []string{"issue.created", "issue.snapshot"} {
		pushedUID, err := uid.New()
		require.NoError(t, err)
		pushed := newRemoteEvent(t, project, &pushedUID, kind, "worker", origin, 800, marshalIssueDocument(t, issueDocumentFields(pushedUID, "worker")))
		require.NoError(t, push(int64(i+1), pushed), "push accepts a complete %s", kind)
		pulledUID, err := uid.New()
		require.NoError(t, err)
		pulled := newRemoteEvent(t, project, &pulledUID, kind, "worker", origin, 801, marshalIssueDocument(t, issueDocumentFields(pulledUID, "worker")))
		inserted, err := store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{pulled})
		require.NoError(t, err, "pull accepts a complete %s", kind)
		require.Equal(t, []bool{true}, inserted)
		stored, err := store.EventsByUIDs(ctx, project.ID, []string{pushed.EventUID, pulled.EventUID})
		require.NoError(t, err)
		require.Len(t, stored, 2)
	}
	return nil
}

// RunRelayRejectsIncompleteIssueDocuments proves relay acceptance applies the
// same document rule as direct federation ingest.
func RunRelayRejectsIncompleteIssueDocuments(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, peer, offer := relayOffers(t, store, "relay-document-project")

	for _, doc := range incompleteIssueDocuments(t, project, "relay-member", peer) {
		doc.rejectsMissing(t, offer(1, doc.event), "relay")
		_, err := store.EventsByUIDs(ctx, project.ID, []string{doc.event.EventUID})
		require.ErrorIs(t, err, db.ErrNotFound, "a rejected %s must not be stored", doc.name)
	}

	for i, kind := range []string{"issue.created", "issue.snapshot"} {
		issueUID, err := uid.New()
		require.NoError(t, err)
		event := newRemoteEvent(t, project, &issueUID, kind, "relay-member", peer, 800, marshalIssueDocument(t, issueDocumentFields(issueUID, "relay-member")))
		require.NoError(t, offer(int64(i+1), event), "relay accepts a complete %s", kind)
		_, err = store.EventsByUIDs(ctx, project.ID, []string{event.EventUID})
		require.NoError(t, err)
	}
}
