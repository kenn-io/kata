package db_test

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestCronRequestResetValidation(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	provenance, err := json.Marshal(db.RootResetProvenance{Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{}})
	require.NoError(t, err)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := uid.FromTime(at)
	require.NoError(t, err)
	valid := `{"kind":"cancel","run_uid":"00000000000000000000000003","requested_by":"operator","requested_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T03:14:05Z"}`
	for _, section := range []string{"events", "entities"} {
		for _, malformed := range []bool{false, true} {
			t.Run(section+"/malformed="+map[bool]string{true: "true", false: "false"}[malformed], func(t *testing.T) {
				value := valid
				if malformed {
					value = `{"kind":"run"}`
				}
				event := db.RemoteEvent{EventUID: "00000000000000000000000004", ProjectUID: pin.ProjectUID, ProjectName: "example-project", OriginInstanceUID: pin.AuthorityUID, Type: "project.metadata_updated", Actor: "operator", HLCPhysicalMS: at.UnixMilli(), CreatedAt: at, Payload: []byte(`{"project_uid":"` + pin.ProjectUID + `","diff":{"cron.request.` + id + `":{"from":null,"to":` + value + `}}}`)}
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
				body, e := db.EncodeRelaySourceEvent(event)
				require.NoError(t, e)
				bodies, e := json.Marshal([][]byte{body})
				require.NoError(t, e)
				snapshot := db.RootResetSnapshot{Events: []byte(`[]`), Entities: []byte(`[]`), Provenance: provenance, Artifacts: []byte(`[]`)}
				if section == "events" {
					snapshot.Events = bodies
				} else {
					snapshot.Entities = bodies
				}
				_, _, e = db.DecodeRootResetPayload(pin, snapshot)
				if malformed {
					require.ErrorIs(t, e, db.ErrFederationIngestValidation)
				} else {
					require.NoError(t, e, "historical valid requests need no receiver-clock freshness")
				}
			})
		}
	}
}
