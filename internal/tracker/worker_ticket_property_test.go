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

// Contract: every signed field is part of execution authority. A change in any
// one field must invalidate the original signature, even if still well formed.
func TestWorkerTicketEveryAuthorityFieldIsSignedProperty(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	changes := []func(*WorkerTicket){
		func(v *WorkerTicket) { v.Version++ },
		func(v *WorkerTicket) { v.Purpose = WorkerPermit },
		func(v *WorkerTicket) { v.KeyID += "-other" },
		func(v *WorkerTicket) { v.RootInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.ProjectUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.OwnerInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.ConnectionUID = v.RunUID },
		func(v *WorkerTicket) { v.Selection.Generation += "-other" },
		func(v *WorkerTicket) { v.Principal.AuthorityInstanceUID = v.RunUID },
		func(v *WorkerTicket) { v.Principal.Kind = SubjectAccount },
		func(v *WorkerTicket) { v.Principal.Subject += "-other" },
		func(v *WorkerTicket) { v.RootActor += "-other" },
		func(v *WorkerTicket) { v.Action = ActionBind },
		func(v *WorkerTicket) { v.Resource += "-other" },
		func(v *WorkerTicket) { v.SourceKey += "-other" },
		func(v *WorkerTicket) { v.Provider += "-other" },
		func(v *WorkerTicket) { v.Method += "-other" },
		func(v *WorkerTicket) { v.RunUID = v.ProjectUID },
		func(v *WorkerTicket) { v.CallUID = v.ProjectUID },
		func(v *WorkerTicket) { v.SessionUID = v.ProjectUID },
		func(v *WorkerTicket) { v.RootEpoch = v.ProjectUID },
		func(v *WorkerTicket) { v.OwnerEpoch = v.ProjectUID },
		func(v *WorkerTicket) { v.ArgumentsDigest = strings.Repeat("b", 64) },
		func(v *WorkerTicket) { v.WriteUID = strings.Repeat("c", 64) },
		func(v *WorkerTicket) { v.ReservationDigest = strings.Repeat("d", 64) },
		func(v *WorkerTicket) { v.ExpiresAt-- },
	}
	hegel.Test(t, func(ht *hegel.T) {
		now := time.Unix(hegel.Draw(ht, hegel.Integers[int64](1, 4000000000)), 0)
		ticket := workerTicketFixture(now)
		ticket.Provider = hegel.Draw(ht, hegel.SampledFrom([]string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"}))
		ticket.Method = ticket.Provider + ".read-status"
		ticket.ExpiresAt = now.Add(time.Duration(hegel.Draw(ht, hegel.Integers(2, 30))) * time.Second).Unix()
		signed, err := SignWorkerTicket(ticket, key)
		require.NoError(ht, err)
		require.NoError(ht, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
		field := hegel.Draw(ht, hegel.Integers(0, len(changes)-1))
		changes[field](&signed.Ticket)
		require.Error(ht, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now), "field %d was unsigned", field)
	}, hegel.WithTestCases(1000))
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
