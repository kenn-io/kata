package db

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
)

// RootKeyTransition authorizes an exact replacement key without replacing the
// project's root identity. Old public keys remain available for historical proof.
type RootKeyTransition struct {
	Version       int        `json:"version"`
	PreviousKeyID string     `json:"previous_key_id"`
	Next          RootKeyPin `json:"next"`
	Signature     []byte     `json:"signature,omitzero"`
}

func rootTransitionBytes(pin RootKeyPin, transition RootKeyTransition) ([]byte, error) {
	if err := ValidateRootKeyPin(pin); err != nil {
		return nil, err
	}
	if err := ValidateRootKeyPin(transition.Next); err != nil {
		return nil, err
	}
	if transition.Version != 1 || transition.PreviousKeyID != pin.KeyID || transition.Next.ProjectUID != pin.ProjectUID || transition.Next.AuthorityUID != pin.AuthorityUID {
		return nil, errors.New("root key transition changes enrolled authority")
	}
	transition.Signature = nil
	return json.Marshal(struct {
		Domain     string            `json:"domain"`
		Transition RootKeyTransition `json:"transition"`
	}{Domain: "kata.root-key-transition.v1", Transition: transition})
}

// SignRootKeyTransition signs an exact replacement key with the currently pinned root key.
func SignRootKeyTransition(pin, next RootKeyPin, private ed25519.PrivateKey) (RootKeyTransition, error) {
	if len(private) != ed25519.PrivateKeySize || RootPublicKeyID(private.Public().(ed25519.PublicKey)) != pin.KeyID {
		return RootKeyTransition{}, errors.New("rotation signing key does not match pinned root")
	}
	transition := RootKeyTransition{Version: 1, PreviousKeyID: pin.KeyID, Next: next}
	canonical, err := rootTransitionBytes(pin, transition)
	if err != nil {
		return RootKeyTransition{}, err
	}
	transition.Signature = ed25519.Sign(private, canonical)
	return transition, nil
}

// VerifyRootKeyTransition verifies replacement-key identity and the current root signature.
func VerifyRootKeyTransition(pin RootKeyPin, transition RootKeyTransition) error {
	canonical, err := rootTransitionBytes(pin, transition)
	if err != nil {
		return err
	}
	if len(transition.Signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(pin.PublicKey), canonical, transition.Signature) {
		return errors.New("invalid root key transition signature")
	}
	return nil
}

// RootKeyTransitionMetadataPrefix scopes public rotation records in the existing
// metadata table. Project exports must not include another project's history.
const RootKeyTransitionMetadataPrefix = "federation.root_key_transition."

// RootKeyTransitionMetadataKey names the retained transition by project and replacement key.
func RootKeyTransitionMetadataKey(transition RootKeyTransition) string {
	return RootKeyTransitionMetadataPrefix + transition.Next.ProjectUID + "." + transition.Next.KeyID
}
