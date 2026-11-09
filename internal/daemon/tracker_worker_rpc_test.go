package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackerRemoteCallCarriesQualifiedSubjectWithoutCredentials(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "rpc.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "rpc-project")
	require.NoError(t, err)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "sync"}
	runtime, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "root-service"}}, NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}))
	require.NoError(t, err)
	snapshot, err := runtime.current()
	require.NoError(t, err)
	descriptor := tracker.PublicDescriptor{Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}, Provider: "notion", Label: "Example account"}
	presence := tracker.WorkerPresence{ProjectUID: project.UID, OwnerInstanceUID: descriptor.Selection.OwnerInstanceUID, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", ExpiresAt: time.Now().Add(time.Minute).Unix(), Connections: []tracker.PublicDescriptor{descriptor}}
	require.NoError(t, runtime.workerBroker.remember(presence, time.Now()))
	run, err := runtime.remoteRun(snapshot, project.UID, p, descriptor.Selection, "notion:source")
	require.NoError(t, err)
	replies := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				replies <- ctx.Err()
				return
			case <-ticker.C:
				commands := runtime.workerBroker.poll(project.UID, descriptor.Selection.OwnerInstanceUID, time.Now())
				if len(commands) == 0 {
					continue
				}
				command := commands[0]
				ticket := command.Ticket.Ticket
				if ticket.Principal != p || ticket.RootActor != "root-service" || command.Ticket.Verify(public, store.InstanceUID(), project.UID, pin.KeyID, tracker.WorkerExecute, time.Now()) != nil {
					replies <- tracker.ErrDenied
					return
				}
				data, _ := json.Marshal(trackerWorkerAuthorization{Allowed: true, Connection: descriptor})
				replies <- runtime.workerBroker.reply(project.UID, descriptor.Selection.OwnerInstanceUID, trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok", Data: data})
				return
			}
		}
	}()
	allowed, err := run.authorize(ctx, tracker.ActionRead)
	require.NoError(t, err)
	require.Equal(t, descriptor, allowed)
	require.NoError(t, <-replies)
	runtime.Suspend()
	_, err = run.authorize(context.WithValue(ctx, trackerRunAuthorityKey{}, trackerRunAuthority{}), tracker.ActionRead)
	require.Error(t, err, "old invocation cannot cross root reload")
}

// Contract: an expired call budget releases volatile admission independently
// of a longer run, while a caller's shorter deadline always wins.
func TestTrackerWorkerReplyBudgetRejectsExpiredCallsWithoutQueueing(t *testing.T) {
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.ExpiresAt = time.Now().Add(-trackerWorkerReplyGrace - time.Second).Unix()
	ctx, cancel := trackerWorkerReplyContext(t.Context(), ticket)
	defer cancel()
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	broker := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	command.Ticket.Ticket = ticket
	_, err := broker.enqueue(ctx, command, false)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, broker.calls)
	shorter, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	ticket.ExpiresAt = time.Now().Add(30 * time.Second).Unix()
	reply, done := trackerWorkerReplyContext(shorter, ticket)
	defer done()
	expected, _ := shorter.Deadline()
	actual, ok := reply.Deadline()
	require.True(t, ok)
	require.Equal(t, expected, actual)
}
