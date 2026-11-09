package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type trackerWorkerRouteFixtureStore struct {
	acknowledgements int
	db.Storage
	pin     db.RootKeyPin
	binding db.FederationBinding
	actor   string
}

func (s *trackerWorkerRouteFixtureStore) RootAuthority(context.Context, string) (db.RootKeyPin, error) {
	return s.pin, nil
}
func (s *trackerWorkerRouteFixtureStore) FederationBindingByProject(context.Context, int64) (db.FederationBinding, error) {
	return s.binding, nil
}
func (s *trackerWorkerRouteFixtureStore) AccessibleProjectUIDs(ctx context.Context, actor string) ([]string, error) {
	s.actor = actor
	return s.Storage.AccessibleProjectUIDs(ctx, actor)
}
func TestTrackerWorkerOwnerUsesOwnMappingAndPinnedRoot(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "owner.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "owner-project")
	require.NoError(t, err)
	const root = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	principal := tracker.Principal{AuthorityInstanceUID: root, Kind: tracker.SubjectService, Subject: "sync"}
	connection := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: root, ProjectUID: project.UID, Principal: principal, Actions: []tracker.Action{tracker.ActionRead}, Sources: []string{"notion:source"}}}}
	runtime, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{connection}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "owner-service"}})
	require.NoError(t, err)
	snapshot, err := runtime.current()
	require.NoError(t, err)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fixture := &trackerWorkerRouteFixtureStore{Storage: store, pin: db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}, binding: db.FederationBinding{Role: db.FederationRoleSpoke, Enabled: true, RelayConfig: &db.RelayBindingConfig{AuthorityUID: root}}}
	runtime.store = fixture
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.RootInstanceUID = root
	ticket.ProjectUID = project.UID
	ticket.Selection = connection.Descriptor(store.InstanceUID()).Selection
	ticket.Principal = principal
	ticket.OwnerEpoch = snapshot.incarnation
	ticket.KeyID = fixture.pin.KeyID
	ticket.RootActor = "root-service"
	ticket.ArgumentsDigest = tracker.WorkerArgumentsDigest([]byte(`{}`))
	ticket.ExpiresAt = time.Now().Add(30 * time.Second).Unix()
	signed, err := tracker.SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	_, selected, err := runtime.authorizeWorkerOwner(ctx, signed, []byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, connection.UID, selected.config.UID)
	require.Equal(t, "owner-service", fixture.actor, "root actor is never an owner-local project bypass")
	signed.Ticket.ArgumentsDigest = tracker.WorkerArgumentsDigest([]byte(`{"other":true}`))
	_, _, err = runtime.authorizeWorkerOwner(ctx, signed, []byte(`{}`))
	require.Error(t, err)
	signed, err = tracker.SignWorkerTicket(ticket, key)
	require.NoError(t, err)
	fixture.binding.Enabled = false
	_, _, err = runtime.authorizeWorkerOwner(ctx, signed, []byte(`{}`))
	require.Error(t, err, "disconnect revokes provider use without touching a provider")
	fixture.binding.Enabled = true
	deniedTicket := ticket
	deniedTicket.Action = tracker.ActionStatus
	deniedTicket.Method = "notion.write-status"
	deniedSigned, err := tracker.SignWorkerTicket(deniedTicket, key)
	require.NoError(t, err)
	owner := newTrackerOwnerWorker(ctx)
	command := trackerWorkerCommand{Ticket: deniedSigned, Arguments: []byte(`{}`)}
	require.True(t, runtime.registerWorkerCall(ctx, owner, command), "valid transport must return a classified denial rather than strand the caller")
	runtime.executeWorkerCall(owner, command, ServerConfig{DB: fixture})
	require.Equal(t, "denied", owner.calls[deniedTicket.CallUID].reply.Code)
	// The already retained denied status reply occupies one ordinary slot.
	// Root cancellation cannot make owner retention bypass its own per-project
	// and per-selection bounds; reserved metadata must still be admissible.
	for i := 0; i < 63; i++ {
		ordinary := ticket
		ordinary.CallUID = workerSyntheticUID(1000 + i)
		signedOrdinary, err := tracker.SignWorkerTicket(ordinary, key)
		require.NoError(t, err)
		require.True(t, runtime.registerWorkerCall(ctx, owner, trackerWorkerCommand{Ticket: signedOrdinary, Arguments: []byte(`{}`)}))
	}
	excess := ticket
	excess.CallUID = workerSyntheticUID(2000)
	signedExcess, err := tracker.SignWorkerTicket(excess, key)
	require.NoError(t, err)
	require.False(t, runtime.registerWorkerCall(ctx, owner, trackerWorkerCommand{Ticket: signedExcess, Arguments: []byte(`{}`)}), "owner project/selection capacity survives root cancellation")
	owner.mu.Lock()
	i := 0
	for _, call := range owner.calls {
		if workerMetadataMethod(call.command.Ticket.Ticket.Method) {
			continue
		}
		call.command.Ticket.Ticket.ProjectUID = workerSyntheticUID(3000 + i)
		call.command.Ticket.Ticket.Selection.Generation = fmt.Sprintf("retained-%d", i)
		i++
	}
	owner.mu.Unlock()
	excess.Selection.Generation = "another-generation"
	signedExcess, err = tracker.SignWorkerTicket(excess, key)
	require.NoError(t, err)
	require.False(t, runtime.registerWorkerCall(ctx, owner, trackerWorkerCommand{Ticket: signedExcess, Arguments: []byte(`{}`)}), "stable owner/connection quota spans projects and retired generations")
	runtime.Suspend()
	_, _, err = runtime.authorizeWorkerOwner(ctx, signed, []byte(`{}`))
	require.Error(t, err, "owner reload suspends retained tickets")
	cp := db.TrackerOwnerCheckpoint{Version: 1, RootInstanceUID: root, ProjectUID: project.UID, OwnerInstanceUID: store.InstanceUID(), Selection: ticket.Selection, Sequence: 1, Head: strings.Repeat("b", 64), WriteUID: strings.Repeat("c", 64), ResultDigest: strings.Repeat("d", 64), Disposition: db.TrackerWriteAcknowledged, Attempt: db.TrackerRemoteAttempt{UID: ticket.CallUID, ArgumentsDigest: ticket.ArgumentsDigest, RootSequence: 1, OwnerSequence: 1, RootEpoch: ticket.RootEpoch, OwnerEpoch: ticket.OwnerEpoch}}
	args, err := json.Marshal(trackerWorkerMetadataArgs{Checkpoint: &cp}, json.Deterministic(true))
	require.NoError(t, err)
	metadataTicket := ticket
	metadataTicket.CallUID = "01ARZ3NDEKTSV4RRFFQ69G5FC0"
	metadataTicket.Purpose = tracker.WorkerAcknowledge
	metadataTicket.Method = "notion.acknowledge"
	metadataTicket.Action = tracker.ActionRecover
	metadataTicket.ArgumentsDigest = tracker.WorkerArgumentsDigest(args)
	metadataTicket.ReservationDigest = metadataTicket.ArgumentsDigest
	metadataTicket.WriteUID = cp.WriteUID
	ackSigned, err := tracker.SignWorkerTicket(metadataTicket, key)
	require.NoError(t, err)
	ackCommand := trackerWorkerCommand{Ticket: ackSigned, Arguments: args}
	require.True(t, runtime.registerWorkerCall(ctx, owner, ackCommand))
	runtime.executeWorkerCall(owner, ackCommand, ServerConfig{DB: fixture})
	require.Equal(t, "ok", owner.calls[metadataTicket.CallUID].reply.Code)
	require.Equal(t, 1, fixture.acknowledgements, "an exact retained acknowledgement needs no active provider snapshot or grant")

}

func (s *trackerWorkerRouteFixtureStore) PrepareTrackerExecutor(context.Context, db.TrackerExecutionFence, string, string) (db.TrackerExecutionFence, error) {
	return db.TrackerExecutionFence{}, tracker.ErrUnavailable
}
func (s *trackerWorkerRouteFixtureStore) AdmitTrackerExecutor(context.Context, db.TrackerExecutionFence, string, string) (db.TrackerExecutionFence, error) {
	return db.TrackerExecutionFence{}, tracker.ErrUnavailable
}
func (s *trackerWorkerRouteFixtureStore) AcknowledgeTrackerExecutor(context.Context, db.TrackerOwnerCheckpoint) error {
	s.acknowledgements++
	return nil
}
