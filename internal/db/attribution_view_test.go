package db_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Structured verification is derived from pinned signature bytes, never a
// caller's verification label. Nil proof is explicitly unverified legacy.
func TestAttributionViewVerification(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: pin.ProjectUID, EventUID: "00000000000000000000000003", ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AuthorityUID: pin.AuthorityUID, AccountableActor: "member", SourceActor: "assistant", Teammate: "researcher", IngressInstanceUID: "00000000000000000000000004", AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, private)
	require.NoError(t, err)
	bytes, err := json.Marshal(map[string]any{"pin": pin, "receipt": receipt})
	require.NoError(t, err)
	var view db.AttributionView
	require.NoError(t, view.Scan(bytes))
	require.Equal(t, "verified", view.Verification)
	require.Equal(t, "member", view.AccountableActor)
	require.Equal(t, "assistant", view.SourceActor)
	require.Equal(t, "researcher", view.Teammate)
	require.Equal(t, pin.AuthorityUID, view.AuthorityUID)
	receipt.AccountableActor = "impostor"
	bytes, err = json.Marshal(map[string]any{"pin": pin, "receipt": receipt})
	require.NoError(t, err)
	require.Error(t, view.Scan(bytes))
	require.NotEqual(t, "verified", view.Verification, "failed verification must clear a reused projection")
	require.Error(t, view.Scan(`{"verification":"verified","accountable_actor":"impostor"}`))
	require.NoError(t, view.Scan(nil))
	require.Equal(t, "legacy", view.Verification)
	require.Empty(t, view.AccountableActor)
	require.NoError(t, view.Scan(`{"pending":true}`))
	view.SourceFallback("assistant", "researcher")
	require.Equal(t, "pending", view.Verification)
	require.Equal(t, "assistant", view.SourceActor)
	require.Empty(t, view.AccountableActor)
	require.Error(t, view.Scan(`{"pending":true,"receipt":{}}`), "pending cannot bypass proof validation")
}
