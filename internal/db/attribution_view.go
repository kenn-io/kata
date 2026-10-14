package db

import (
	"encoding/json/v2"
	"errors"
)

// PendingCreationMetadataPrefix is local creation intent, not signed proof.
// Project exports omit it; trusted owner backups retain it through compaction.
const PendingCreationMetadataPrefix = "federation.pending_creation."

// AttributionView is a server-derived creation projection. Raw author fields
// remain source labels; only a verified root receipt supplies accountability.
type AttributionView struct {
	AccountableActor string `json:"accountable_actor,omitempty"`
	SourceActor      string `json:"source_actor,omitempty"`
	Teammate         string `json:"teammate,omitempty"`
	AuthorityUID     string `json:"authority_uid,omitempty"`
	Verification     string `json:"verification,omitempty" enum:"verified,pending,legacy"`
}

// Scan consumes the receipt and its stored pin from the same native read.
// It verifies the signature before exposing a verified status and clears prior
// state even when a reused destination fails. No transported label grants proof.
func (view *AttributionView) Scan(value any) error {
	*view = AttributionView{Verification: "legacy"}
	if value == nil {
		return nil
	}
	var data []byte
	switch value := value.(type) {
	case []byte:
		data = value
	case string:
		data = []byte(value)
	default:
		return errors.New("invalid stored attribution projection")
	}
	var proof struct {
		Pin     RootKeyPin          `json:"pin"`
		Receipt *AttributionReceipt `json:"receipt"`
		Pending bool                `json:"pending"`
	}
	if err := json.Unmarshal(data, &proof); err != nil {
		return err
	}
	if proof.Pending && proof.Receipt == nil {
		view.Verification = "pending"
		return nil
	}
	if proof.Receipt == nil {
		return errors.New("missing stored attribution receipt")
	}
	if err := VerifyRootReceipt(proof.Pin, *proof.Receipt); err != nil {
		return err
	}
	*view = AttributionView{AccountableActor: proof.Receipt.AccountableActor, SourceActor: proof.Receipt.SourceActor, Teammate: proof.Receipt.Teammate, AuthorityUID: proof.Receipt.AuthorityUID, Verification: "verified"}
	return nil
}

// SourceFallback retains source labels when no verified creation receipt is available.
func (view *AttributionView) SourceFallback(author, teammate string) {
	if view.Verification == "verified" {
		return
	}
	view.SourceActor = author
	view.Teammate = teammate
	if view.Verification == "" {
		view.Verification = "legacy"
	}
}
