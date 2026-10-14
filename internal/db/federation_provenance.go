package db

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
)

// RootKeyPin is the trusted authority for one project. Receiving a public key
// alongside a receipt is insufficient: enrollment must establish this pin.
// Signing keys are owner-only files and never enter this portable record.
type RootKeyPin struct {
	ProjectUID   string `json:"project_uid"`
	AuthorityUID string `json:"authority_uid"`
	KeyID        string `json:"key_id"`
	PublicKey    []byte `json:"public_key"`
	Retired      bool   `json:"retired,omitzero"`
}

// AttributionReceipt proves root acceptance independently of immutable source
// events. It does not authenticate the physical identity of a declared agent.
// Verification is derived from the pinned key, never a field supplied by clients.
type AttributionReceipt struct {
	Version            int       `json:"version"`
	ProjectUID         string    `json:"project_uid"`
	EventUID           string    `json:"event_uid"`
	ContentHash        string    `json:"content_hash"`
	AuthorityUID       string    `json:"authority_uid"`
	AccountableActor   string    `json:"accountable_actor"`
	SourceActor        string    `json:"source_actor"`
	Teammate           string    `json:"teammate"`
	IngressInstanceUID string    `json:"ingress_instance_uid"`
	AcceptedAt         time.Time `json:"accepted_at"`
	ResetEpoch         int64     `json:"reset_epoch"`
	Sequence           int64     `json:"sequence"`
	KeyID              string    `json:"key_id"`
	Signature          []byte    `json:"signature,omitzero"`
}

// RootPublicKeyID identifies exact public-key bytes without a private secret.
func RootPublicKeyID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

// receiptSigningBytes defines the v1 canonical transcript: the fixed domain
// followed by the ordered receipt fields with a UTC timestamp and no signature.
// All numbers are integers. No map iteration or source payload reserialization
// participates in signing; the immutable source content hash is the commitment.
func receiptSigningBytes(receipt AttributionReceipt) ([]byte, error) {
	if receipt.Version != 1 || !uid.Valid(receipt.ProjectUID) || !uid.Valid(receipt.EventUID) || !uid.Valid(receipt.AuthorityUID) || !uid.Valid(receipt.IngressInstanceUID) || receipt.ResetEpoch <= 0 || receipt.Sequence <= 0 || receipt.AcceptedAt.IsZero() {
		return nil, errors.New("invalid root attribution identity or sequence")
	}
	if err := ValidateTokenActor(receipt.AccountableActor); err != nil {
		return nil, fmt.Errorf("invalid accountable actor: %w", err)
	}
	if strings.TrimSpace(receipt.SourceActor) == "" {
		return nil, errors.New("missing source actor")
	}
	if err := teammate.Validate(receipt.Teammate); err != nil {
		return nil, fmt.Errorf("invalid attribution teammate: %w", err)
	}
	for _, value := range []string{receipt.ContentHash, receipt.KeyID} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size || value != strings.ToLower(value) {
			return nil, errors.New("invalid attribution digest")
		}
	}
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	receipt.Signature = nil
	return json.Marshal(struct {
		Domain  string             `json:"domain"`
		Receipt AttributionReceipt `json:"receipt"`
	}{Domain: "kata.root-attribution.v1", Receipt: receipt})
}

// SignRootReceipt returns a signed copy. The caller must derive the accountable
// actor from authenticated ingress before calling; source labels grant no access.
func SignRootReceipt(receipt AttributionReceipt, privateKey ed25519.PrivateKey) (AttributionReceipt, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return AttributionReceipt{}, errors.New("invalid root private key")
	}
	if receipt.KeyID != RootPublicKeyID(privateKey.Public().(ed25519.PublicKey)) {
		return AttributionReceipt{}, errors.New("root signing key does not match key ID")
	}
	canonical, err := receiptSigningBytes(receipt)
	if err != nil {
		return AttributionReceipt{}, err
	}
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	receipt.Signature = ed25519.Sign(privateKey, canonical)
	return receipt, nil
}

// VerifyRootReceipt checks the enrolled project/authority/key and every signed
// declaration. Malformed keys and signatures reject without reaching Verify.
func VerifyRootReceipt(pin RootKeyPin, receipt AttributionReceipt) error {
	if len(pin.PublicKey) != ed25519.PublicKeySize || pin.KeyID != RootPublicKeyID(pin.PublicKey) || receipt.KeyID != pin.KeyID || receipt.AuthorityUID != pin.AuthorityUID || receipt.ProjectUID != pin.ProjectUID || len(receipt.Signature) != ed25519.SignatureSize {
		return errors.New("root attribution does not match pinned authority")
	}
	canonical, err := receiptSigningBytes(receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pin.PublicKey), canonical, receipt.Signature) {
		return errors.New("invalid root attribution signature")
	}
	return nil
}
