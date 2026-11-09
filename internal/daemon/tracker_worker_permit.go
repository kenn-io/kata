package daemon

import (
	"encoding/json/v2"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func workerReservationDigest(root db.TrackerExecutionFence) (string, error) {
	if root.Validate() != nil || root.Role != "coordinator" || root.Remote == nil || root.Phase != db.TrackerWritePrepared || root.ActivationRequired {
		return "", db.ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(root, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return tracker.WorkerArgumentsDigest(raw), nil
}
func workerPermitDigest(root, owner db.TrackerExecutionFence) (string, error) {
	if !db.TrackerOwnerProofMatchesReservation(root, owner) || root.Phase != db.TrackerWritePrepared || owner.Phase != db.TrackerWritePrepared || root.ActivationRequired || owner.ActivationRequired {
		return "", db.ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(struct {
		Domain      string
		Root, Owner db.TrackerExecutionFence
	}{"kata:tracker-permit-preparations:v1", root, owner}, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return tracker.WorkerArgumentsDigest(raw), nil
}
func verifyWorkerContinuation(signed tracker.SignedWorkerTicket, original tracker.WorkerTicket, pin db.RootKeyPin, purpose tracker.WorkerPurpose, write, digest string) error {
	if signed.Verify(pin.PublicKey, pin.AuthorityUID, pin.ProjectUID, pin.KeyID, purpose, time.Now()) != nil {
		return tracker.ErrDenied
	}
	expected := original
	expected.Purpose = purpose
	expected.KeyID = pin.KeyID
	expected.WriteUID = write
	expected.ReservationDigest = digest
	expected.ExpiresAt = signed.Ticket.ExpiresAt
	if expected != signed.Ticket {
		return tracker.ErrDenied
	}
	return nil
}
