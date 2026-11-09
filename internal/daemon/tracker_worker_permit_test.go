package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"
)

func workerRootReservationFixture(t *testing.T) (tracker.WorkerTicket, db.TrackerExecutionFence, db.TrackerExecutionFence) {
	t.Helper()
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.Method = "notion.write-status"
	ticket.Action = tracker.ActionStatus
	root := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: ticket.RootInstanceUID, ProjectUID: ticket.ProjectUID, Connection: ticket.Selection, Sequence: 1, PendingEventUID: ticket.CallUID, SourceKey: ticket.Resource, Locator: "item", Target: "closed", ProcessEpoch: ticket.RootEpoch, Phase: db.TrackerWritePrepared, Remote: &db.TrackerRemoteAttempt{UID: ticket.CallUID, ArgumentsDigest: ticket.ArgumentsDigest, RootSequence: 1, OwnerSequence: 1, RootEpoch: ticket.RootEpoch, OwnerEpoch: ticket.OwnerEpoch}}
	var err error
	root.WriteUID, err = db.TrackerStatusWriteUID(root.RootInstanceUID, root.ProjectUID, root.SourceKey, root.Locator, root.PendingEventUID, root.Target)
	require.NoError(t, err)
	owner, err := db.TrackerExecutorPreparation(root, "01ARZ3NDEKTSV4RRFFQ69G5FB4")
	require.NoError(t, err)
	return ticket, root, owner
}
func TestTrackerWorkerPermitBindsBothCompletePreparations(t *testing.T) {
	ticket, root, owner := workerRootReservationFixture(t)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ticket.KeyID = db.RootPublicKeyID(public)
	digest, err := workerPermitDigest(root, owner)
	require.NoError(t, err)
	permit := ticket
	permit.Purpose = tracker.WorkerPermit
	permit.WriteUID = root.WriteUID
	permit.ReservationDigest = digest
	permit.ExpiresAt = time.Now().Add(5 * time.Second).Unix()
	signed, err := tracker.SignWorkerTicket(permit, key)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: ticket.ProjectUID, AuthorityUID: ticket.RootInstanceUID, KeyID: ticket.KeyID, PublicKey: public}
	require.NoError(t, verifyWorkerContinuation(signed, ticket, pin, tracker.WorkerPermit, root.WriteUID, digest))
	changed := owner
	changed.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB5"
	changedDigest, err := workerPermitDigest(root, changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, changedDigest, "head hash alone omits the owner's process incarnation")
	require.Error(t, verifyWorkerContinuation(signed, ticket, pin, tracker.WorkerPermit, root.WriteUID, changedDigest))
	changedTicket := ticket
	changedTicket.SessionUID = ticket.RunUID
	require.Error(t, verifyWorkerContinuation(signed, changedTicket, pin, tracker.WorkerPermit, root.WriteUID, digest))
	require.Error(t, verifyWorkerContinuation(signed, ticket, pin, tracker.WorkerReserve, root.WriteUID, digest))
}
