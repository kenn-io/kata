package db

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"strings"
)

// BoundSpokeClaimPrincipal returns the holder tuple a spoke presents to its
// upstream hub. Relays bind the incoming hop identity into a local opaque
// client kind before replacing the account with their upstream account.
func BoundSpokeClaimPrincipal(binding FederationBinding, principal ClaimPrincipal, localInstanceUID string) ClaimPrincipal {
	if binding.Role != FederationRoleSpoke {
		return principal
	}
	actor := strings.TrimSpace(binding.Actor)
	if actor == "" {
		return principal
	}
	if binding.RelayConfig != nil {
		identity, _ := json.Marshal([3]string{principal.HolderInstanceUID, principal.Holder, principal.ClientKind})
		digest := sha256.Sum256(append([]byte("kata:relay-claim-holder:v1\x00"), identity...))
		principal.HolderInstanceUID = localInstanceUID
		principal.ClientKind = "relay:v1:" + base64.RawURLEncoding.EncodeToString(digest[:])
		principal.Holder = actor
		return principal
	}
	if principal.AuthenticatedHost {
		ownerDigest := sha256.Sum256([]byte("kata:spoke-host-claim-owner:v1\x00" + principal.Holder))
		principal.ClientKind = "spoke-host:v1:" + base64.RawURLEncoding.EncodeToString(ownerDigest[:])
	}
	principal.Holder = actor
	return principal
}
