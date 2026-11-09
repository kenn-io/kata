package tracker

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: a well-formed change to an execution ticket's authority must
// invalidate the original signature.
func TestWorkerTicketValidAuthorityChangesInvalidateSignatureProperty(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	changes := []func(*WorkerTicket){
		func(v *WorkerTicket) { v.KeyID += "-other" },
		func(v *WorkerTicket) { v.RootInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.ProjectUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.OwnerInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.ConnectionUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.Generation += "-other" },
		func(v *WorkerTicket) { v.Principal.AuthorityInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.Principal.Kind = SubjectService },
		func(v *WorkerTicket) { v.Principal.Subject += "-other" },
		func(v *WorkerTicket) { v.RootActor += "-other" },
		func(v *WorkerTicket) { v.Action = ActionBind },
		func(v *WorkerTicket) { v.Resource += "-other" },
		func(v *WorkerTicket) { v.SourceKey += "-other" },
		func(v *WorkerTicket) { v.Method = v.Provider + ".read-status" },
		func(v *WorkerTicket) { v.RunUID = v.ProjectUID },
		func(v *WorkerTicket) { v.CallUID = v.ProjectUID },
		func(v *WorkerTicket) { v.SessionUID = v.ProjectUID },
		func(v *WorkerTicket) { v.RootEpoch = v.ProjectUID },
		func(v *WorkerTicket) { v.OwnerEpoch = v.ProjectUID },
		func(v *WorkerTicket) { v.ArgumentsDigest = strings.Repeat("b", 64) },
		func(v *WorkerTicket) { v.ExpiresAt-- },
	}
	hegel.Test(t, func(ht *hegel.T) {
		now := time.Unix(hegel.Draw(ht, hegel.Integers[int64](1, 4000000000)), 0)
		ticket := workerTicketFixture(now)
		ticket.Principal.Kind = SubjectAccount
		ticket.Provider = hegel.Draw(ht, hegel.SampledFrom([]string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"}))
		ticket.Method = ticket.Provider + ".authorize"
		ticket.ExpiresAt = now.Add(time.Duration(hegel.Draw(ht, hegel.Integers(2, 30))) * time.Second).Unix()
		signed, err := SignWorkerTicket(ticket, key)
		require.NoError(ht, err)
		require.NoError(ht, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
		field := hegel.Draw(ht, hegel.Integers(0, len(changes)-1))
		changes[field](&signed.Ticket)
		require.NoError(ht, signed.Ticket.Validate(), "field %d must remain a valid ticket", field)
		require.Error(ht, signed.Verify(public, signed.Ticket.RootInstanceUID, signed.Ticket.ProjectUID, signed.Ticket.KeyID, signed.Ticket.Purpose, now), "field %d was unsigned", field)
	}, hegel.WithTestCases(1000))
}

// Contract: the two permit digests are authenticated even when changed to
// other well-formed values.
func TestWorkerPermitDigestsAreSignedProperty(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		now := time.Unix(1900000000, 0)
		ticket := workerTicketFixture(now)
		ticket.Purpose, ticket.Action, ticket.Method = WorkerPermit, ActionStatus, "notion.write-status"
		ticket.WriteUID, ticket.ReservationDigest = strings.Repeat("c", 64), strings.Repeat("d", 64)
		ticket.ExpiresAt = now.Add(5 * time.Second).Unix()
		signed, err := SignWorkerTicket(ticket, key)
		require.NoError(ht, err)
		require.NoError(ht, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerPermit, now))
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 0 {
			signed.Ticket.WriteUID = strings.Repeat("e", 64)
		} else {
			signed.Ticket.ReservationDigest = strings.Repeat("f", 64)
		}
		require.NoError(ht, signed.Ticket.Validate())
		require.Error(ht, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerPermit, now))
	}, hegel.WithTestCases(100))
}

// Contract: all four purposes expire at their exact Unix second; expiry never
// authorizes a new dispatch or erases retained native evidence.
func TestWorkerTicketExpiryProperty(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		now := time.Unix(1900000000, 0)
		ticket := workerTicketFixture(now)
		ticket.Purpose = hegel.Draw(ht, hegel.SampledFrom([]WorkerPurpose{WorkerExecute, WorkerReserve, WorkerPermit, WorkerAcknowledge}))
		limit := 30
		switch ticket.Purpose {
		case WorkerReserve, WorkerPermit:
			ticket.Action, ticket.Method = ActionStatus, "notion.write-status"
			ticket.WriteUID, ticket.ReservationDigest = strings.Repeat("c", 64), strings.Repeat("d", 64)
			if ticket.Purpose == WorkerPermit {
				limit = 5
			}
		case WorkerAcknowledge:
			ticket.Action, ticket.Method = ActionRecover, "notion.acknowledge"
			ticket.WriteUID, ticket.ReservationDigest = strings.Repeat("c", 64), ticket.ArgumentsDigest
		}
		seconds := hegel.Draw(ht, hegel.Integers(-1, 31))
		ticket.ExpiresAt = now.Add(time.Duration(seconds) * time.Second).Unix()
		signed, err := SignWorkerTicket(ticket, key)
		require.NoError(ht, err)
		got := signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, ticket.Purpose, now)
		require.Equal(ht, seconds > 0 && seconds <= limit, got == nil)
	}, hegel.WithTestCases(500))
}
