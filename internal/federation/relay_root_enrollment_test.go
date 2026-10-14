package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R3/R4: initial trusted enrollment validates the whole public-key chain before
// mutation. Later metadata cannot inject an unknown historical signing key.
func TestRelayInitialRootPinValidation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			node := newRelayMatrixNode(t, backend, "member")
			for _, name := range []string{"protocol", "signature", "authority", "valid"} {
				t.Run(name, func(t *testing.T) {
					project, err := node.store.CreateProject(t.Context(), "enrollment-"+name)
					require.NoError(t, err)
					oldPublic, oldPrivate, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					nextPublic, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					old := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(oldPublic), PublicKey: oldPublic}
					next := old
					next.KeyID, next.PublicKey = db.RootPublicKeyID(nextPublic), nextPublic
					transition, err := db.SignRootKeyTransition(old, next, oldPrivate)
					require.NoError(t, err)
					old.Retired = true
					handshake := api.RelayHandshake{ProtocolVersion: db.RelayProtocolVersion, Root: next, EnrollmentRoot: &old, RootKeyTransitions: []db.RootKeyTransition{transition}}
					switch name {
					case "protocol":
						handshake.ProtocolVersion = 0
					case "signature":
						handshake.RootKeyTransitions[0].Signature[0] ^= 1
					case "authority":
						handshake.Root.AuthorityUID = "00000000000000000000000003"
					}
					err = federation.PinRelayRootAuthority(t.Context(), node.store, handshake)
					if name != "valid" {
						require.Error(t, err, "invalid initial chain must fail before pinning any key")
						_, err := node.store.RootAuthority(t.Context(), project.UID)
						require.ErrorIs(t, err, db.ErrNotFound)
						return
					}
					require.NoError(t, err)
					current, err := node.store.RootAuthority(t.Context(), project.UID)
					require.NoError(t, err)
					require.Equal(t, next, current)
					keys := map[string]bool{}
					for record, err := range node.store.ExportAttribution(t.Context(), db.ExportFilter{ProjectID: &project.ID}) {
						require.NoError(t, err)
						if key, ok := record.(*db.RootKeyPin); ok {
							keys[key.KeyID] = key.Retired
						}
					}
					require.Equal(t, map[string]bool{old.KeyID: true, next.KeyID: false}, keys)
					require.NoError(t, federation.PinRelayRootAuthority(t.Context(), node.store, handshake), "exact retry retains current pin and history")
					unknownPublic, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					injected := old
					injected.KeyID, injected.PublicKey = db.RootPublicKeyID(unknownPublic), unknownPublic
					handshake.EnrollmentRoot = &injected
					require.NoError(t, federation.PinRelayRootAuthority(t.Context(), node.store, handshake), "existing pin controls subsequent metadata, not a newly supplied historical key")
					count := 0
					for record, err := range node.store.ExportAttribution(t.Context(), db.ExportFilter{ProjectID: &project.ID}) {
						require.NoError(t, err)
						if _, ok := record.(*db.RootKeyPin); ok {
							count++
						}
					}
					require.Equal(t, 2, count)
				})
			}
		})
	}
}
