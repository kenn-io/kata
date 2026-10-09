package db

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"iter"
	"slices"

	"go.kenn.io/kata/internal/uid"
)

// RootAttributionSigner is owner-local transient signing material. It is never
// part of an API, transport record or database backup.
type RootAttributionSigner struct {
	AuthorityUID string
	PrivateKey   ed25519.PrivateKey
}

// AttributionStorage keeps proofs separately from compactable source history.
// Callers must use authenticated enrollment IDs, never caller-supplied actors.
type AttributionStorage interface {
	RotateRootAuthority(context.Context, RootKeyTransition) error
	RootKeyTransitions(context.Context, string) ([]RootKeyTransition, error)
	PinRootAuthority(context.Context, RootKeyPin) error
	RootAuthority(context.Context, string) (RootKeyPin, error)
	RecordRootAttribution(context.Context, int64, RemoteEvent, RootAttributionSigner) (AttributionReceipt, error)
	ApplyUpstreamAttribution(context.Context, RootKeyPin, AttributionReceipt) error
	EntityAttribution(context.Context, string, string, string) (AttributionReceipt, error)
	AttributionReceiptsAfter(context.Context, string, int64, int64, int) ([]AttributionReceipt, error)
	ExportAttribution(context.Context, ExportFilter) iter.Seq2[ImportRecord, error]
}

// EntityProvenance records original creation, not the most recent editor.
type EntityProvenance struct {
	ProjectUID string `json:"project_uid"`
	Kind       string `json:"kind"`
	EntityUID  string `json:"entity_uid"`
	EventUID   string `json:"event_uid"`
}

// ImportKind identifies pinned public root keys in owner backups.
func (*RootKeyPin) ImportKind() string { return "federation_root_key" }

// ImportKind identifies signed root acceptance receipts in owner backups.
func (*AttributionReceipt) ImportKind() string { return "federation_event_provenance" }

// ImportKind identifies retained entity creation attribution in owner backups.
func (*EntityProvenance) ImportKind() string { return "federation_entity_provenance" }

// ValidateRootKeyPin checks project, authority and public key identity.
func ValidateRootKeyPin(pin RootKeyPin) error {
	if !uid.Valid(pin.ProjectUID) || !uid.Valid(pin.AuthorityUID) || len(pin.PublicKey) != ed25519.PublicKeySize || pin.KeyID != RootPublicKeyID(pin.PublicKey) {
		return errors.New("invalid root authority pin")
	}
	return nil
}

// ProjectAttributionVisible intersects the native request grant before queries.
func ProjectAttributionVisible(ctx context.Context, projectUID string) bool {
	allowed, restricted := AuthorizedProjects(ctx)
	return !restricted || slices.Contains(allowed, projectUID)
}

// EventCreationProvenance reads source declarations without changing bytes/hash.
// Legacy snapshots carry no creation proof; reset manifests provide explicit refs.
func EventCreationProvenance(event RemoteEvent) (string, []EntityProvenance, error) {
	var payload struct {
		Metadata   JSONBlob `json:"metadata"`
		Teammate   string   `json:"teammate"`
		CommentUID string   `json:"comment_uid"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", nil, err
	}
	switch event.Type {
	case "issue.created":
		if event.IssueUID == nil || !uid.Valid(*event.IssueUID) {
			return "", nil, errors.New("creation event missing issue UID")
		}
		handle, _ := IssueTeammate(payload.Metadata)
		return handle, []EntityProvenance{{ProjectUID: event.ProjectUID, Kind: "issue", EntityUID: *event.IssueUID, EventUID: event.EventUID}}, nil
	case "issue.commented":
		if !uid.Valid(payload.CommentUID) {
			return "", nil, errors.New("comment creation missing UID")
		}
		return payload.Teammate, []EntityProvenance{{ProjectUID: event.ProjectUID, Kind: "comment", EntityUID: payload.CommentUID, EventUID: event.EventUID}}, nil
	default:
		return "", nil, nil
	}
}
