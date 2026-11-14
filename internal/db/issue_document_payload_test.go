package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const documentIssueUID = "01HZZZZZZZZZZZZZZZZZZZZZZA"

// fullIssueDocument is a creation or snapshot payload with every field the
// writers always send, as raw JSON fields a test can remove or replace.
func fullIssueDocument() map[string]jsontext.Value {
	return map[string]jsontext.Value{
		"uid":        jsontext.Value(`"` + documentIssueUID + `"`),
		"short_id":   jsontext.Value(`"zzza"`),
		"title":      jsontext.Value(`"Example issue"`),
		"body":       jsontext.Value(`""`),
		"author":     jsontext.Value(`"worker"`),
		"status":     jsontext.Value(`"open"`),
		"metadata":   jsontext.Value(`{}`),
		"created_at": jsontext.Value(`"2026-05-23T12:00:00.000Z"`),
	}
}

func issueDocumentEvent(t *testing.T, kind string, fields map[string]jsontext.Value) RemoteEvent {
	t.Helper()
	payload, err := json.Marshal(fields)
	require.NoError(t, err)
	header := documentIssueUID
	return RemoteEvent{Type: kind, IssueUID: &header, Payload: payload}
}

func TestRemoteEventIssueUIDAcceptsACompleteIssueDocument(t *testing.T) {
	withOptionalFields := fullIssueDocument()
	withOptionalFields["owner"] = jsontext.Value(`"worker"`)
	withOptionalFields["priority"] = jsontext.Value(`2`)
	withOptionalFields["revision"] = jsontext.Value(`7`)
	withOptionalFields["labels"] = jsontext.Value(`["bug"]`)
	withOptionalFields["links"] = jsontext.Value(`[{"type":"blocks","to_issue_uid":"01HZZZZZZZZZZZZZZZZZZZZZZB","to_short_id":"zzzb","author":"worker","created_at":"2026-05-23T12:00:01.000Z"}]`)
	withOptionalFields["comments"] = jsontext.Value(`[{"comment_uid":"01HZZZZZZZZZZZZZZZZZZZZZZC","author":"worker","body":"note","created_at":"2026-05-23T12:00:02.000Z"}]`)

	for name, fields := range map[string]map[string]jsontext.Value{
		"required fields alone, with an empty body": fullIssueDocument(),
		"optional fields as writers send them":      withOptionalFields,
	} {
		for _, kind := range []string{"issue.created", "issue.snapshot"} {
			got, err := RemoteEventIssueUID(issueDocumentEvent(t, kind, fields))
			require.NoError(t, err, "%s %s", kind, name)
			assert.Equal(t, documentIssueUID, got)
		}
	}
}

func TestRemoteEventIssueUIDRejectsAnIssueDocumentWithoutARequiredField(t *testing.T) {
	for _, field := range []string{"uid", "short_id", "title", "body", "author", "status", "metadata", "created_at"} {
		t.Run("without "+field, func(t *testing.T) {
			fields := fullIssueDocument()
			delete(fields, field)
			_, err := RemoteEventIssueUID(issueDocumentEvent(t, "issue.snapshot", fields))
			require.ErrorIs(t, err, ErrFederationIngestValidation)
			require.ErrorContains(t, err, "has no "+field)
		})
	}
}

func TestRemoteEventIssueUIDRejectsIssueDocumentValuesNoWriterSends(t *testing.T) {
	for name, replace := range map[string]map[string]jsontext.Value{
		"empty short_id":       {"short_id": jsontext.Value(`""`)},
		"empty title":          {"title": jsontext.Value(`""`)},
		"empty author":         {"author": jsontext.Value(`""`)},
		"empty status":         {"status": jsontext.Value(`""`)},
		"empty created_at":     {"created_at": jsontext.Value(`""`)},
		"null body":            {"body": jsontext.Value(`null`)},
		"metadata as a string": {"metadata": jsontext.Value(`"{}"`)},
		"metadata as null":     {"metadata": jsontext.Value(`null`)},
		"title as a number":    {"title": jsontext.Value(`7`)},
	} {
		t.Run(name, func(t *testing.T) {
			fields := fullIssueDocument()
			maps.Copy(fields, replace)
			_, err := RemoteEventIssueUID(issueDocumentEvent(t, "issue.snapshot", fields))
			require.ErrorIs(t, err, ErrFederationIngestValidation)
		})
	}
}

// Event content hashes cover the payload bytes, so the marshaled field order
// of an issue document is wire format. This pins it with every field set.
func TestIssueDocumentPayloadMarshalsInWireOrder(t *testing.T) {
	text := func(s string) *string { return &s }
	priority := int64(2)
	payload, err := json.Marshal(IssueDocumentPayload{
		UID: documentIssueUID, ShortID: "zzza", Title: "Example issue", Body: "text", Author: "worker",
		Owner: text("owner"), AssignmentExpiresOn: text("2026-05-30"), Priority: &priority,
		Status: "closed", ClosedReason: text("done"), ClosedAt: text("2026-05-24T12:00:00.000Z"),
		DeletedAt: text("2026-05-25T12:00:00.000Z"), Metadata: jsontext.Value(`{"team":"core"}`),
		Labels: []string{"bug"},
		Links: []IssueDocumentLink{{
			Type: "blocks", ToShortID: "zzzb", ToIssueUID: "01HZZZZZZZZZZZZZZZZZZZZZZB", Incoming: true,
			Author: "worker", CreatedAt: "2026-05-23T12:00:01.000Z",
		}},
		Comments: []IssueDocumentComment{{
			ReplyToUID: "01HZZZZZZZZZZZZZZZZZZZZZZD", ReplyKind: "answer", EditedAt: "2026-05-23T12:00:03.000Z",
			CommentUID: "01HZZZZZZZZZZZZZZZZZZZZZZC", Author: "worker", Teammate: "helper",
			Body: "note", CreatedAt: "2026-05-23T12:00:02.000Z",
		}},
		CreatedAt: "2026-05-23T12:00:00.000Z", UpdatedAt: "2026-05-26T12:00:00.000Z", Revision: 7,
		IdempotencyKey: "key", IdempotencyFingerprint: "fingerprint", RecurrenceUID: "01HZZZZZZZZZZZZZZZZZZZZZZR",
		OccurrenceKey: "occurrence", Source: "importer", ExternalID: "external-1",
	})
	require.NoError(t, err)
	assert.Equal(t, `{"uid":"01HZZZZZZZZZZZZZZZZZZZZZZA","short_id":"zzza","title":"Example issue","body":"text","author":"worker",`+
		`"owner":"owner","assignment_expires_on":"2026-05-30","priority":2,"status":"closed","closed_reason":"done",`+
		`"closed_at":"2026-05-24T12:00:00.000Z","deleted_at":"2026-05-25T12:00:00.000Z","metadata":{"team":"core"},"labels":["bug"],`+
		`"links":[{"type":"blocks","to_short_id":"zzzb","to_issue_uid":"01HZZZZZZZZZZZZZZZZZZZZZZB","incoming":true,"author":"worker",`+
		`"created_at":"2026-05-23T12:00:01.000Z"}],`+
		`"comments":[{"reply_to_uid":"01HZZZZZZZZZZZZZZZZZZZZZZD","reply_kind":"answer","edited_at":"2026-05-23T12:00:03.000Z",`+
		`"comment_uid":"01HZZZZZZZZZZZZZZZZZZZZZZC","author":"worker","teammate":"helper","body":"note","created_at":"2026-05-23T12:00:02.000Z"}],`+
		`"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-26T12:00:00.000Z","revision":7,"idempotency_key":"key",`+
		`"idempotency_fingerprint":"fingerprint","recurrence_uid":"01HZZZZZZZZZZZZZZZZZZZZZZR","occurrence_key":"occurrence",`+
		`"source":"importer","external_id":"external-1"}`, string(payload))
}
