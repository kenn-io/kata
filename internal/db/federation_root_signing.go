package db

import (
	"context"
	"crypto/ed25519"
	"fmt"
)

// RootSigningKeyError reports that this daemon cannot sign root receipts for a
// project because its owner-only signing key is missing or no longer matches
// the project's pin. The write that needed the receipt does not commit.
type RootSigningKeyError struct {
	Project string
}

func (e *RootSigningKeyError) Error() string {
	return fmt.Sprintf("project %s needs this daemon's root signing key (federation-root.key), "+
		"which is missing or does not match the project's pin; restore the key file from backup "+
		"and restart the daemon, or run `kata federation root-key repin --project %s`",
		e.Project, e.Project)
}

// RootSignerKeyID returns the public key ID of loaded signing material, or ""
// when the signer carries no usable private key.
func RootSignerKeyID(signer RootAttributionSigner) string {
	if len(signer.PrivateKey) != ed25519.PrivateKeySize {
		return ""
	}
	return RootPublicKeyID(signer.PrivateKey.Public().(ed25519.PublicKey))
}

// RootSignerMatches reports whether signer holds the private key for pin.
func RootSignerMatches(pin RootKeyPin, signer RootAttributionSigner) bool {
	return signer.AuthorityUID == pin.AuthorityUID && RootSignerKeyID(signer) == pin.KeyID
}

// RootRepinStrandsRelaysError reports that an unsigned repin was refused
// because the project still has active relay enrollments pinned to the
// previous key and the caller did not accept stranding them.
type RootRepinStrandsRelaysError struct {
	Enrollments int64
}

func (e *RootRepinStrandsRelaysError) Error() string {
	return fmt.Sprintf("%d active relay enrollment(s) are pinned to the previous key; an unsigned repin leaves them unable to sync the project", e.Enrollments)
}

// RootRepinStorage replaces a local root project's active pin without a signed
// transition. It is the explicit owner recovery for a lost signing key: the
// previous key stays retained for historical receipts. Downstream relays that
// pinned it cannot follow this change, so the replacement counts the project's
// active enrollments inside its own transaction and refuses with
// RootRepinStrandsRelaysError unless strandRelays accepts that loss.
type RootRepinStorage interface {
	RepinRootAuthority(ctx context.Context, previousKeyID string, next RootKeyPin, strandRelays bool) error
}
