package db_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R5: every signed transition must authorize the exact next root key. All
// input seed bytes and mutation offsets vary; no network or new PBT dependency.
func FuzzRootKeyTransitionTampering(f *testing.F) {
	f.Add([]byte("original-root"), []byte("next-root"), uint64(0))
	f.Add([]byte{}, []byte{0, 255}, uint64(31))
	f.Fuzz(func(t *testing.T, oldSeed, newSeed []byte, offset uint64) {
		a, b := sha256.Sum256(oldSeed), sha256.Sum256(newSeed)
		oldPrivate, nextPrivate := ed25519.NewKeyFromSeed(a[:]), ed25519.NewKeyFromSeed(b[:])
		oldPublic, nextPublic := oldPrivate.Public().(ed25519.PublicKey), nextPrivate.Public().(ed25519.PublicKey)
		pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(oldPublic), PublicKey: oldPublic}
		next := pin
		next.KeyID, next.PublicKey = db.RootPublicKeyID(nextPublic), nextPublic
		transition, err := db.SignRootKeyTransition(pin, next, oldPrivate)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootKeyTransition(pin, transition))
		// Alter a next-key byte while preserving every source authority field.
		transition.Next.PublicKey = append([]byte(nil), transition.Next.PublicKey...)
		transition.Next.PublicKey[offset%uint64(len(transition.Next.PublicKey))] ^= 1
		require.Error(t, db.VerifyRootKeyTransition(pin, transition), "a key rotation may never authorize other key bytes")
	})
}
func TestRootKeyTransitionRejectsOtherAuthority(t *testing.T) {
	seed := sha256.Sum256([]byte("root"))
	private := ed25519.NewKeyFromSeed(seed[:])
	public := private.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	next := pin
	next.AuthorityUID = "00000000000000000000000003"
	_, err := db.SignRootKeyTransition(pin, next, private)
	require.Error(t, err)
}
