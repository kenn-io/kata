package db

// RelayResetSupersededMetadataPrefix scopes the per-enrollment record a relay
// writes when a root or forwarded reset prepares a downstream grant's next epoch.
// Keys are the prefix, the project UID, a dot and the downstream binding UID.
const RelayResetSupersededMetadataPrefix = "relay_reset_superseded."

// RelayResetSupersededMetadataKey names the superseded-delivery record for one
// downstream enrollment of a project.
func RelayResetSupersededMetadataKey(projectUID, bindingUID string) string {
	return RelayResetSupersededMetadataPrefix + projectUID + "." + bindingUID
}

// RelayResetSupersededDeliveries records the event and artifact deliveries a
// downstream had not acknowledged when its root captured or relay installed a
// root reset. The checkpoint carries the root's authoritative state for them, so
// those outbox rows stay as history and are never offered again, neither in
// the superseded epoch nor by the activation that leaves it.
//
// Receipt deliveries are never superseded. A receipt is root-issued proof the
// downstream may need before it is allowed to install the forwarded checkpoint
// at all, and storing one twice is harmless, so receipts stay owed in the old
// epoch and follow the grant into the new one like any other late delivery.
type RelayResetSupersededDeliveries struct {
	// Epoch is the downstream epoch whose unacknowledged rows were superseded.
	Epoch int64 `json:"epoch"`
	// NextEpoch is the prepared checkpoint's downstream epoch.
	NextEpoch int64 `json:"next_epoch"`
	// SnapshotUID identifies the root checkpoint that superseded the rows.
	SnapshotUID string `json:"snapshot_uid"`
	// ThroughID bounds the retained event/artifact prefix the checkpoint covers.
	ThroughID int64 `json:"through_id"`
	Events    int   `json:"events"`
	Artifacts int   `json:"artifacts"`
}

// RelayDeliverySupersedable reports whether a forwarded checkpoint may stand
// in for an unacknowledged downstream delivery, or the delivery may simply
// stay owed. Artifact offers are reseeded from the installed projection and
// receipts are root-issued and stay owed, so neither blocks. Event deliveries
// block unless the root already accepted their source, through a retained
// receipt or a receipt in the checkpoint being installed.
func RelayDeliverySupersedable(stream, sourceUID, sourceHash string, hasReceipt bool, rootAccepted map[string]string) bool {
	if stream == RelayStreamArtifact || stream == RelayStreamReceipt {
		return true
	}
	return hasReceipt || rootAccepted[sourceUID] == sourceHash
}

// BoundaryFor returns the outbox row ID at or below which rows of stream in
// epoch are superseded, for a read of that epoch (requested == epoch) or the
// activation that leaves it (requested == NextEpoch). Receipts are never
// superseded, and any other epoch pair, including a record left by an earlier
// transition, hides nothing.
func (r RelayResetSupersededDeliveries) BoundaryFor(stream string, epoch, requested int64) int64 {
	if stream == RelayStreamReceipt || r.Epoch != epoch || (requested != epoch && requested != r.NextEpoch) {
		return 0
	}
	return r.ThroughID
}
