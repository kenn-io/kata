package db_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R5 requires root proof bound to the immutable source identity and authenticated
// accountable actor. Neither source labels nor a client verification flag prove it.
func TestRootAttributionSignature(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	receipt := db.AttributionReceipt{Version: 1, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: "00000000000000000000000003", ContentHash: strings.Repeat("a", 64), AccountableActor: "member", SourceActor: "assistant", Teammate: "researcher", IngressInstanceUID: "00000000000000000000000004", AcceptedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ResetEpoch: 1, Sequence: 1}
	signed, err := db.SignRootReceipt(receipt, private)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, signed))
	assert.Empty(t, receipt.Signature, "signing must leave source declaration intact")
	assert.Equal(t, receipt.SourceActor, signed.SourceActor)
	assert.Equal(t, receipt.ContentHash, signed.ContentHash)
	again, err := db.SignRootReceipt(receipt, private)
	require.NoError(t, err)
	assert.Equal(t, signed, again, "canonical signatures must be deterministic")
	for _, tc := range []struct {
		name   string
		change func(*db.AttributionReceipt)
	}{
		{"version", func(r *db.AttributionReceipt) { r.Version++ }},
		{"project", func(r *db.AttributionReceipt) { r.ProjectUID = "00000000000000000000000009" }},
		{"authority", func(r *db.AttributionReceipt) { r.AuthorityUID = "00000000000000000000000009" }},
		{"source event", func(r *db.AttributionReceipt) { r.EventUID = "00000000000000000000000009" }},
		{"hash", func(r *db.AttributionReceipt) { r.ContentHash = strings.Repeat("b", 64) }},
		{"account", func(r *db.AttributionReceipt) { r.AccountableActor = "other-member" }},
		{"source actor", func(r *db.AttributionReceipt) { r.SourceActor = "other-assistant" }},
		{"teammate", func(r *db.AttributionReceipt) { r.Teammate = "other-researcher" }},
		{"ingress", func(r *db.AttributionReceipt) { r.IngressInstanceUID = "00000000000000000000000009" }},
		{"time", func(r *db.AttributionReceipt) { r.AcceptedAt = r.AcceptedAt.Add(time.Millisecond) }},
		{"epoch", func(r *db.AttributionReceipt) { r.ResetEpoch++ }},
		{"sequence", func(r *db.AttributionReceipt) { r.Sequence++ }},
		{"key", func(r *db.AttributionReceipt) { r.KeyID = strings.Repeat("b", 64) }},
		{"signature", func(r *db.AttributionReceipt) { r.Signature[0] ^= 1 }},
		{"truncated signature", func(r *db.AttributionReceipt) { r.Signature = r.Signature[:63] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := signed
			altered.Signature = slices.Clone(signed.Signature)
			tc.change(&altered)
			assert.Error(t, db.VerifyRootReceipt(pin, altered))
		})
	}
	otherPublic, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, err = db.SignRootReceipt(receipt, otherPrivate)
	assert.Error(t, err, "wrong private key cannot issue under a pinned key ID")
	otherPin := pin
	otherPin.PublicKey = otherPublic
	assert.Error(t, db.VerifyRootReceipt(otherPin, signed))
	shortPin := pin
	shortPin.PublicKey = public[:31]
	assert.Error(t, db.VerifyRootReceipt(shortPin, signed), "malformed keys must reject without panic")
}
