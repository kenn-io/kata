package client_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/federationpublication"
	"go.kenn.io/kata/pkg/client/generated"
)

var (
	_ generated.SetProjectAccessRequestBodyVisibility = generated.All
	_ generated.SetProjectAccessRequestBodyVisibility = generated.Teams
)

// These assignments keep the existing exported project-policy constants
// source-compatible after publication adds its independent visibility enum.
func TestGeneratedPublicationPreservesRecordNumbers(t *testing.T) {
	document := dbtest.PublicationFixture(t, "01K00000000000000000000006")
	document.Digest = ""
	issue := document.Records[1].(*db.IssueExport)
	issue.Metadata = jsontext.Value(`{"decimal":1.0,"large":9007199254740993}`)
	var event *db.EventExport
	for _, record := range document.Records {
		if candidate, ok := record.(*db.EventExport); ok && candidate.IssueUID != nil && *candidate.IssueUID == issue.UID {
			event = candidate
			break
		}
	}
	require.NotNil(t, event)
	var payload map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(event.Payload, &payload))
	payload["metadata"] = issue.Metadata
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)
	event.Payload = payloadBytes
	require.NoError(t, db.PrepareReplayEvent(event, document.ProjectUID, event.ProjectName, true))
	original, err := federationpublication.Encode(document)
	require.NoError(t, err)

	var body generated.PublishFederationProjectBody
	require.NoError(t, json.Unmarshal(original, &body.Envelope))
	require.NoError(t, body.Validate())

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"decimal":1.0`)
	require.Contains(t, string(raw), `"large":9007199254740993`)
}

func TestGeneratedPublicationPreservesEnvelope(t *testing.T) {
	document := dbtest.PublicationFixture(t, "01K00000000000000000000006")
	document.Digest = ""
	original, err := federationpublication.Encode(document)
	require.NoError(t, err)
	var body generated.PublishFederationProjectBody
	require.NoError(t, json.Unmarshal(original, &body.Envelope))
	// false requests a commit and must be present on the wire.
	require.NoError(t, body.Validate())
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	var wire struct {
		Preflight *bool          `json:"preflight"`
		Envelope  jsontext.Value `json:"envelope"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.NotNil(t, wire.Preflight)
	require.False(t, *wire.Preflight)
	decoded, err := federationpublication.Decode(wire.Envelope)
	require.NoError(t, err)
	canonical, err := federationpublication.Encode(decoded)
	require.NoError(t, err)
	require.Equal(t, original, canonical)
}
