package tracker

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: transport enrollment is insufficient; only the pinned root key and
// exact purpose-bound call may authorize owner execution. No credentials exist
// in this fixture or in the signed control envelope.
func TestWorkerTicketBindsPurposePrincipalAndExactCall(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(1900000000, 0)
	ticket := workerTicketFixture(now)
	signed, err := SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	require.NoError(t, signed.Verify(pub, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
	require.Error(t, signed.Verify(pub, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerPermit, now))
	require.Error(t, signed.Verify(pub, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now.Add(31*time.Second)))
	for _, change := range []func(*WorkerTicket){
		func(t *WorkerTicket) { t.Selection.Generation = "other" },
		func(t *WorkerTicket) { t.Principal.AuthorityInstanceUID = t.Selection.OwnerInstanceUID },
		func(t *WorkerTicket) { t.RootActor = "other-actor" },
		func(t *WorkerTicket) { t.ArgumentsDigest = strings.Repeat("b", 64) },
		func(t *WorkerTicket) { t.Method = "notion.content" },
		func(t *WorkerTicket) { t.SourceKey = "notion:other" },
		func(t *WorkerTicket) { t.OwnerEpoch = t.RunUID },
		func(t *WorkerTicket) { t.Purpose = WorkerPermit },
	} {
		changed := signed
		change(&changed.Ticket)
		require.Error(t, changed.Verify(pub, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.Error(t, signed.Verify(other.Public().(ed25519.PublicKey), ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
	ticket.Purpose = WorkerPermit
	ticket.Action = ActionStatus
	ticket.Method = "notion.write-status"
	ticket.WriteUID = strings.Repeat("c", 64)
	ticket.ReservationDigest = strings.Repeat("d", 64)
	ticket.ExpiresAt = now.Add(6 * time.Second).Unix()
	_, err = SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	permit, err := SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	require.Error(t, permit.Verify(pub, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerPermit, now), "permit lifetime is bounded to five seconds plus clock granularity")
}
func workerTicketFixture(now time.Time) WorkerTicket {
	return WorkerTicket{Version: 1, Purpose: WorkerExecute, KeyID: "root-key", RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Selection: Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Principal: Principal{AuthorityInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Kind: SubjectService, Subject: "sync"}, RootActor: "root-service", Action: ActionRead, Resource: "notion:data-source", Provider: "notion", Method: "notion.pages", RunUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", CallUID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", SessionUID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", RootEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2", OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB3", ArgumentsDigest: strings.Repeat("a", 64), ExpiresAt: now.Add(30 * time.Second).Unix()}
}
func TestWorkerMethodAllowlistCannotProxyURLsOrInventEffects(t *testing.T) {
	for _, provider := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		require.True(t, WorkerMethodAllowed(provider, provider+".read-status", ActionRead))
		require.True(t, WorkerMethodAllowed(provider, provider+".write-status", ActionStatus))
		require.False(t, WorkerMethodAllowed(provider, provider+".write-status", ActionRead))
		require.False(t, WorkerMethodAllowed(provider, "https://daemon.example/private", ActionRead))
		require.False(t, WorkerMethodAllowed(provider, provider+".publish", ActionStatus))
	}
}

func TestWorkerAcknowledgementHasItsOwnNonExecutionDomain(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	ticket := workerTicketFixture(now)
	ticket.Purpose = WorkerAcknowledge
	ticket.Method = "notion.acknowledge"
	ticket.Action = ActionRecover
	ticket.WriteUID = strings.Repeat("c", 64)
	ticket.ReservationDigest = ticket.ArgumentsDigest
	signed, err := SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	require.NoError(t, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerAcknowledge, now))
	require.Error(t, signed.Verify(public, ticket.RootInstanceUID, ticket.ProjectUID, ticket.KeyID, WorkerExecute, now))
	ticket.Purpose = WorkerExecute
	ticket.WriteUID = ""
	ticket.ReservationDigest = ""
	_, err = SignWorkerTicket(ticket, key)
	require.Error(t, err, "an execution-domain ticket cannot stand in for a receipt acknowledgement")
}
