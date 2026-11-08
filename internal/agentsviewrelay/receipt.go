package agentsviewrelay

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// Receipt proves root-observed receiver acceptance, never indexing. Fields are
// ordered for v1 signing and key material always comes from an enrolled pin.
type Receipt struct {
	Version        int       `json:"v"`
	ProjectUID     string    `json:"project_uid"`
	AuthorityUID   string    `json:"authority_uid"`
	ServiceID      string    `json:"service_id"`
	IssueUID       string    `json:"issue_uid"`
	CloseEventUID  string    `json:"close_event_uid"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	SnapshotBytes  int64     `json:"snapshot_bytes"`
	Agent          string    `json:"agent"`
	SessionID      string    `json:"session_id"`
	Locator        string    `json:"locator"`
	Status         string    `json:"status"`
	AcceptedAt     time.Time `json:"accepted_at"`
	KeyID          string    `json:"key_id"`
	Signature      []byte    `json:"signature,omitempty"`
}

func receiptBytes(r Receipt) ([]byte, error) {
	if r.Version != 1 || !uid.Valid(r.ProjectUID) || !uid.Valid(r.AuthorityUID) || !uid.Valid(r.IssueUID) || !uid.Valid(r.CloseEventUID) || !ValidDigest(r.ServiceID) || !ValidDigest(r.SnapshotSHA256) || !ValidDigest(r.KeyID) || r.SnapshotBytes <= 0 || r.SnapshotBytes > MaxSnapshotBytes || r.Agent == "" || r.SessionID == "" || r.Locator == "" || r.AcceptedAt.IsZero() || (r.Status != "created" && r.Status != "unchanged") {
		return nil, errors.New("invalid AgentsView receipt")
	}
	r.Signature = nil
	r.AcceptedAt = r.AcceptedAt.UTC()
	return json.Marshal(struct {
		Domain  string  `json:"domain"`
		Receipt Receipt `json:"receipt"`
	}{"kata.project-trace-receipt.v1", r})
}

func SignReceipt(r Receipt, key ed25519.PrivateKey) (Receipt, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Receipt{}, errors.New("AgentsView receipt signer unavailable")
	}
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	r.KeyID = hex.EncodeToString(sum[:])
	b, err := receiptBytes(r)
	if err != nil {
		return Receipt{}, err
	}
	r.Signature = ed25519.Sign(key, b)
	return r, nil
}

func VerifyReceipt(r Receipt, key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize || len(r.Signature) != ed25519.SignatureSize {
		return errors.New("invalid AgentsView receipt key or signature")
	}
	sum := sha256.Sum256(key)
	if r.KeyID != hex.EncodeToString(sum[:]) {
		return errors.New("AgentsView receipt differs from pinned root")
	}
	b, err := receiptBytes(r)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, b, r.Signature) {
		return errors.New("invalid AgentsView receipt signature")
	}
	return nil
}
