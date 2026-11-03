package db_test

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestDecodeRootResetPayloadAcceptsCanonicalizedCommentReference(t *testing.T) {
	projectUID, err := uid.New()
	require.NoError(t, err)
	rootUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	commentUID = strings.ToLower(commentUID)

	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: projectUID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	createdAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeEvent := func(eventType string, eventIssueUID *string, payload string) db.RemoteEvent {
		eventUID, err := uid.New()
		require.NoError(t, err)
		event := db.RemoteEvent{
			EventUID: eventUID, OriginInstanceUID: rootUID, ProjectUID: projectUID,
			ProjectName: "example-project", IssueUID: eventIssueUID,
			Type: eventType, Actor: "assistant", HLCPhysicalMS: createdAt.UnixMilli(),
			CreatedAt: createdAt, Payload: jsontext.Value(payload),
		}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		return event
	}

	issueEvent := makeEvent("issue.created", &issueUID,
		fmt.Sprintf(`{"uid":%q,"title":"Finding","author":"assistant","metadata":{}}`, issueUID))
	commentEvent := makeEvent("issue.commented", &issueUID,
		fmt.Sprintf(`{"comment_uid":%q,"author":"assistant","teammate":"reviewer","body":"Comment"}`, commentUID))
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{
		Version: 1, ProjectUID: projectUID, EventUID: commentEvent.EventUID, ContentHash: commentEvent.ContentHash,
		AuthorityUID: rootUID, AccountableActor: "member", SourceActor: commentEvent.Actor, Teammate: "reviewer",
		IngressInstanceUID: rootUID, AcceptedAt: createdAt, ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID,
	}, private)
	require.NoError(t, err)

	sources := make([][]byte, 0, 2)
	for _, event := range []db.RemoteEvent{issueEvent, commentEvent} {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		sources = append(sources, body)
	}
	provenance := db.RootResetProvenance{
		Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{receipt},
		Entities: []db.EntityProvenance{{ProjectUID: projectUID, Kind: "comment", EntityUID: commentUID, EventUID: commentEvent.EventUID}},
	}
	snapshot := db.RootResetSnapshot{Artifacts: []byte(`[]`)}
	snapshot.Events, err = json.Marshal(sources)
	require.NoError(t, err)
	snapshot.Entities, err = json.Marshal([][]byte{})
	require.NoError(t, err)
	snapshot.Provenance, err = json.Marshal(provenance)
	require.NoError(t, err)

	_, decoded, err := db.DecodeRootResetPayload(pin, snapshot)
	require.NoError(t, err, "a retained lowercase creation reference must resolve the canonicalized comment projection")
	require.Contains(t, decoded.Entities, provenance.Entities[0], "validation must preserve the signed reference spelling")
}
