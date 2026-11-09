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
	"testing"
	"time"
)

func TestRemoteServiceCandidatesAreRootConfiguredAndBounded(t *testing.T) {
	const root = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	const other = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	snapshot := &trackerRuntimeSnapshot{mappings: []config.TrackerAccessMapping{{ProjectUID: project, Principal: tracker.Principal{AuthorityInstanceUID: other, Kind: tracker.SubjectService, Subject: "same-name"}, Actor: "remote"}, {ProjectUID: project, Principal: tracker.Principal{AuthorityInstanceUID: root, Kind: tracker.SubjectAccount, Subject: "member"}, Actor: "member"}}}
	candidates, err := trackerRemoteServiceCandidates(snapshot, root, project)
	require.NoError(t, err)
	require.Empty(t, candidates)
	for i := 0; i < 16; i++ {
		snapshot.mappings = append(snapshot.mappings, config.TrackerAccessMapping{ProjectUID: project, Principal: tracker.Principal{AuthorityInstanceUID: root, Kind: tracker.SubjectService, Subject: fmt.Sprintf("service-%02d", i)}, Actor: "root-service"})
	}
	candidates, err = trackerRemoteServiceCandidates(snapshot, root, project)
	require.NoError(t, err)
	require.Len(t, candidates, 16)
	snapshot.mappings = append(snapshot.mappings, config.TrackerAccessMapping{ProjectUID: project, Principal: tracker.Principal{AuthorityInstanceUID: root, Kind: tracker.SubjectService, Subject: "excess"}, Actor: "root-service"})
	_, err = trackerRemoteServiceCandidates(snapshot, root, project)
	require.Error(t, err, "truncation must not silently choose an actor")
}

func TestRemoteBindingSelectionDoesNotGuessAmongServiceCandidates(t *testing.T) {
	for _, test := range []struct {
		name  string
		codes map[string]string
		want  error
	}{
		{"denied", map[string]string{"service-a": "denied", "service-b": "denied"}, tracker.ErrDenied},
		{"ambiguous", map[string]string{"service-a": "ok", "service-b": "ok"}, tracker.ErrAmbiguous},
		{"unavailable", map[string]string{"service-a": "ok", "service-b": "unavailable"}, tracker.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "routing.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			project, err := store.CreateProject(ctx, "routing-project")
			require.NoError(t, err)
			public, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			mappings := []config.TrackerAccessMapping{}
			for _, name := range []string{"service-a", "service-b"} {
				mappings = append(mappings, config.TrackerAccessMapping{ProjectUID: project.UID, Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: name}, Actor: name})
			}
			runtime, err := NewTrackerRuntime(ctx, store, nil, mappings, NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}))
			require.NoError(t, err)
			snapshot, err := runtime.current()
			require.NoError(t, err)
			descriptor := tracker.PublicDescriptor{Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}, Provider: "notion", Label: "Example account"}
			require.NoError(t, runtime.workerBroker.remember(tracker.WorkerPresence{ProjectUID: project.UID, OwnerInstanceUID: descriptor.Selection.OwnerInstanceUID, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", ExpiresAt: time.Now().Add(time.Minute).Unix(), Connections: []tracker.PublicDescriptor{descriptor}}, time.Now()))
			go func() {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						for _, command := range runtime.workerBroker.poll(project.UID, descriptor.Selection.OwnerInstanceUID, time.Now()) {
							ticket := command.Ticket.Ticket
							code := test.codes[ticket.Principal.Subject]
							reply := trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: code}
							if code == "ok" {
								reply.Data, _ = json.Marshal(trackerWorkerAuthorization{Allowed: true, Connection: descriptor})
							}
							_ = runtime.workerBroker.reply(project.UID, descriptor.Selection.OwnerInstanceUID, reply)
						}
					}
				}
			}()
			_, err = runtime.routeRemoteBinding(ctx, snapshot, db.IssueSyncBinding{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source"}, descriptor.Selection)
			require.ErrorIs(t, err, test.want)
		})
	}
}
