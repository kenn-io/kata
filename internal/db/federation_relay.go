package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// Relay constants bound protocol version, stream names, hop depth and envelope bytes.
const (
	RelayProtocolVersion = 1
	RelayStreamEvent     = "events"
	RelayStreamReceipt   = "receipts"
	RelayStreamArtifact  = "artifacts"
	MaxRelayHubs         = 8
	// A cross-branch path can visit the source leaf and both maximum-depth hub
	// paths. The paths share their root, so this is one leaf plus 2*MaxRelayHubs-1 hubs.
	// Hub depth is enforced separately on each enrolled authority path.
	MaxRelayPathNodes     = 2 * MaxRelayHubs
	MaxRelayEnvelopeBytes = 64 << 20
)

// RelayHopAuthority is resolved from the live enrolled transport. An envelope
// digest detects corruption; it never establishes any of these permissions.
type RelayHopAuthority struct {
	BindingUID          string `json:"binding_uid"`
	ProjectUID          string `json:"project_uid"`
	AuthorityUID        string `json:"authority_uid"`
	SenderInstanceUID   string `json:"sender_instance_uid"`
	ReceiverInstanceUID string `json:"receiver_instance_uid"`
	Epoch               int64  `json:"epoch"`
}

// RelayEnvelope keeps hop sequencing separate from immutable source identity.
// Body is opaque bytes so forwarding cannot reserialize source payloads. Artifact
// offers carry the complete artifact's manifest; SourceHash commits its vectors.
type RelayEnvelope struct {
	Version             int      `json:"version"`
	BindingUID          string   `json:"binding_uid"`
	ProjectUID          string   `json:"project_uid"`
	AuthorityUID        string   `json:"authority_uid"`
	SenderInstanceUID   string   `json:"sender_instance_uid"`
	ReceiverInstanceUID string   `json:"receiver_instance_uid"`
	Epoch               int64    `json:"epoch"`
	Sequence            int64    `json:"sequence"`
	Stream              string   `json:"stream"`
	Path                []string `json:"path"`
	SourceUID           string   `json:"source_uid"`
	SourceHash          string   `json:"source_hash"`
	Body                []byte   `json:"body"`
	Digest              string   `json:"digest"`
}

// RelayStreamCursors are sequence numbers in one explicitly named namespace.
// Root reset baselines and hop translations use separate instances of this type.
type RelayStreamCursors struct {
	Events    int64 `json:"events"`
	Receipts  int64 `json:"receipts"`
	Artifacts int64 `json:"artifacts"`
}

func (c RelayStreamCursors) valid() bool { return c.Events >= 0 && c.Receipts >= 0 && c.Artifacts >= 0 }

func validRelayDigest(value string) bool {
	if len(value) != 2*sha256.Size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func supportedRelayStream(stream string) bool {
	return stream == RelayStreamEvent || stream == RelayStreamReceipt || stream == RelayStreamArtifact
}
func validateRelayHop(authority RelayHopAuthority) error {
	for _, value := range []string{authority.BindingUID, authority.ProjectUID, authority.AuthorityUID, authority.SenderInstanceUID, authority.ReceiverInstanceUID} {
		if !uid.Valid(value) {
			return errors.New("invalid relay hop identity")
		}
	}
	if authority.SenderInstanceUID == authority.ReceiverInstanceUID || authority.Epoch <= 0 {
		return errors.New("invalid relay hop epoch or self-link")
	}
	return nil
}
func envelopeAuthority(e RelayEnvelope) RelayHopAuthority {
	return RelayHopAuthority{BindingUID: e.BindingUID, ProjectUID: e.ProjectUID, AuthorityUID: e.AuthorityUID, SenderInstanceUID: e.SenderInstanceUID, ReceiverInstanceUID: e.ReceiverInstanceUID, Epoch: e.Epoch}
}
func relayEnvelopeDigest(envelope RelayEnvelope) (string, error) {
	if err := validateRelayHop(envelopeAuthority(envelope)); err != nil {
		return "", err
	}
	if envelope.Version != RelayProtocolVersion || envelope.Sequence <= 0 || !supportedRelayStream(envelope.Stream) || !validRelayDigest(envelope.SourceHash) || len(envelope.Body) > MaxRelayEnvelopeBytes {
		return "", errors.New("invalid relay stream, sequence, source digest or size")
	}
	if envelope.Stream == RelayStreamArtifact {
		if !ValidRelayArtifactSourceUID(envelope.SourceUID, envelope.SourceHash) {
			return "", errors.New("artifact offer identity must bind its exact digest")
		}
	} else if !uid.Valid(envelope.SourceUID) {
		return "", errors.New("invalid relay source UID")
	}
	if len(envelope.Path) == 0 || len(envelope.Path) > MaxRelayPathNodes || envelope.Path[len(envelope.Path)-1] != envelope.SenderInstanceUID {
		return "", errors.New("invalid relay path")
	}
	seen := make(map[string]bool, len(envelope.Path))
	for _, instance := range envelope.Path {
		if !uid.Valid(instance) || seen[instance] || instance == envelope.ReceiverInstanceUID {
			return "", errors.New("relay path has a cycle or invalid identity")
		}
		seen[instance] = true
	}
	bodyDigest := sha256.Sum256(envelope.Body)
	// The header commits exact body bytes without allocating their JSON/base64
	// representation during verification. The on-wire Digest is not self-signed.
	header := envelope
	header.Body = nil
	header.Digest = ""
	canonical, err := json.Marshal(struct {
		Domain   string        `json:"domain"`
		Header   RelayEnvelope `json:"header"`
		BodyHash string        `json:"body_hash"`
	}{"kata.relay-hop.v1", header, hex.EncodeToString(bodyDigest[:])})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// SealRelayEnvelope computes the immutable hop commitment.
func SealRelayEnvelope(envelope RelayEnvelope) (RelayEnvelope, error) {
	digest, err := relayEnvelopeDigest(envelope)
	if err != nil {
		return RelayEnvelope{}, fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	envelope.Body = slices.Clone(envelope.Body)
	envelope.Path = slices.Clone(envelope.Path)
	envelope.Digest = digest
	return envelope, nil
}

// ValidateRelayEnvelope checks the live hop identity, path bounds and source digest.
func ValidateRelayEnvelope(authority RelayHopAuthority, envelope RelayEnvelope) error {
	if err := validateRelayHop(authority); err != nil {
		return err
	}
	if envelopeAuthority(envelope) != authority {
		return fmt.Errorf("%w: relay envelope does not match authenticated hop", ErrFederationIngestValidation)
	}
	digest, err := relayEnvelopeDigest(envelope)
	if err != nil || !validRelayDigest(envelope.Digest) || digest != envelope.Digest {
		return fmt.Errorf("%w: invalid relay envelope digest or content", ErrFederationIngestValidation)
	}
	return nil
}

// ValidRelayArtifactSourceUID binds an offer to its unchanged artifact digest.
// Restoration adds the immutable restore event UID so a retired offer does not
// suppress new delivery. Both forms retain their exact identity across retries.
func ValidRelayArtifactSourceUID(sourceUID, digest string) bool {
	base, restoredBy, restored := strings.Cut(sourceUID, ":")
	return validRelayDigest(digest) && base == digest && (!restored || uid.Valid(restoredBy))
}
