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
			require.ErrorContains(t, err, field)
		})
	}
}

func TestRemoteEventIssueUIDRejectsIssueDocumentValuesNoWriterSends(t *testing.T) {
	for name, replace := range map[string]map[string]jsontext.Value{
		"empty short_id":        {"short_id": jsontext.Value(`""`)},
		"empty title":           {"title": jsontext.Value(`""`)},
		"empty author":          {"author": jsontext.Value(`""`)},
		"empty status":          {"status": jsontext.Value(`""`)},
		"empty created_at":      {"created_at": jsontext.Value(`""`)},
		"null body":             {"body": jsontext.Value(`null`)},
		"metadata as a string":  {"metadata": jsontext.Value(`"{}"`)},
		"metadata as null":      {"metadata": jsontext.Value(`null`)},
		"uid unlike the header": {"uid": jsontext.Value(`"01HZZZZZZZZZZZZZZZZZZZZZZB"`)},
		"title as a number":     {"title": jsontext.Value(`7`)},
	} {
		t.Run(name, func(t *testing.T) {
			fields := fullIssueDocument()
			maps.Copy(fields, replace)
			_, err := RemoteEventIssueUID(issueDocumentEvent(t, "issue.snapshot", fields))
			require.ErrorIs(t, err, ErrFederationIngestValidation)
		})
	}
}
