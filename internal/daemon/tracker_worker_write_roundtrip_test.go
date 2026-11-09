package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/tracker"
)

// Exercise the real root coordinator, original native claim/event checks,
// signed reserve/permit, exact proof acceptance and checkpoint transaction.
// The scripted owner uses synthetic provider results; owner native CAS and
// HTTP transport authorization have separate conformance/handler contracts.
func TestTrackerRootRemoteWriteRetainsOnlyCompletedOwnerResponse(t *testing.T) {
	for _, scenario := range []string{"completed", "proven-not-sent", "reply-lost-after-permit"} {
		t.Run(scenario, func(t *testing.T) {
			notSent := scenario == "proven-not-sent"
			lostReply := scenario == "reply-lost-after-permit"
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "root.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			project, err := store.CreateProject(ctx, "root-project")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped task", Author: "member"})
			require.NoError(t, err)
			public, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			principal := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "sync"}
			runtime, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "root-service"}}, NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}))
			require.NoError(t, err)
			selection := tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}
			binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
			require.NoError(t, err)
			mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ExternalID: "page:item", ObjectType: "issue", IssueID: &issue.ID})
			require.NoError(t, err)
			_, err = store.ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, runtime.epoch)
			require.NoError(t, err)
			_, _, _, err = store.CloseIssueWithEvents(ctx, issue.ID, "done", "member", "Completed task", nil)
			require.NoError(t, err)
			at := time.Now().UTC().Truncate(time.Millisecond)
			_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", at, at.Add(-30*time.Minute))
			require.NoError(t, err)
			require.True(t, claimed)
			guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: at, BindingUpdatedAt: &binding.UpdatedAt}
			joined, err := store.IssueStatusMappingByID(ctx, guard, mapping.ID)
			require.NoError(t, err)
			descriptor := tracker.PublicDescriptor{Selection: selection, Provider: "notion", Label: "Example account"}
			snapshot, err := runtime.current()
			require.NoError(t, err)
			require.NoError(t, runtime.workerBroker.remember(tracker.WorkerPresence{ProjectUID: project.UID, OwnerInstanceUID: selection.OwnerInstanceUID, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", ExpiresAt: time.Now().Add(time.Minute).Unix(), Connections: []tracker.PublicDescriptor{descriptor}}, time.Now()))
			run, err := runtime.remoteRun(snapshot, project.UID, principal, selection, binding.SourceKey)
			require.NoError(t, err)
			run.source = binding.SourceKey
			routed, err := runtime.mappedContext(ctx, snapshot, project.UID, principal)
			require.NoError(t, err)
			routed = context.WithValue(routed, trackerRunAuthorityKey{}, trackerRunAuthority{root: store.InstanceUID(), project: project.UID, source: binding.SourceKey, principal: principal, action: tracker.ActionRead, incarnation: snapshot.incarnation, remote: run})
			routed = tracker.WithRunSelection(routed, selection)
			callCtx, cancelCall := context.WithCancel(routed)
			defer cancelCall()
			done := make(chan error, 1)
			go func() {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				seen := map[string]bool{}
				for {
					select {
					case <-ctx.Done():
						done <- ctx.Err()
						return
					case <-ticker.C:
						for _, command := range runtime.workerBroker.poll(project.UID, selection.OwnerInstanceUID, time.Now()) {
							ticket := command.Ticket.Ticket
							if seen[ticket.CallUID] {
								continue
							}
							seen[ticket.CallUID] = true
							reply := trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok"}
							if strings.HasSuffix(ticket.Method, ".authorize") {
								reply.Data, _ = json.Marshal(trackerWorkerAuthorization{Allowed: true, Connection: descriptor})
								if err := runtime.workerBroker.reply(project.UID, selection.OwnerInstanceUID, reply); err != nil {
									done <- err
									return
								}
								continue
							}
							if ticket.Method != "notion.write-status" {
								done <- tracker.ErrInvalid
								return
							}
							if err := command.Ticket.Verify(public, store.InstanceUID(), project.UID, pin.KeyID, tracker.WorkerExecute, time.Now()); err != nil {
								done <- err
								return
							}
							signal := trackerWorkerSignal{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Stage: "prepare"}
							reserve, err := runtime.workerBroker.writeSignal(ctx, project.UID, selection.OwnerInstanceUID, ticket.OwnerEpoch, signal)
							if err != nil {
								done <- err
								return
							}
							digest, err := workerReservationDigest(reserve.Reservation)
							if err != nil {
								done <- err
								return
							}
							if err := verifyWorkerContinuation(reserve.Authority, ticket, pin, tracker.WorkerReserve, reserve.Reservation.WriteUID, digest); err != nil {
								done <- err
								return
							}
							owner, err := db.TrackerExecutorPreparation(reserve.Reservation, "01ARZ3NDEKTSV4RRFFQ69G5FB1")
							if err != nil {
								done <- err
								return
							}
							signal.Stage = "permit"
							signal.Owner = &owner
							permit, err := runtime.workerBroker.writeSignal(ctx, project.UID, selection.OwnerInstanceUID, ticket.OwnerEpoch, signal)
							if err != nil {
								done <- err
								return
							}
							digest, err = workerPermitDigest(reserve.Reservation, owner)
							if err != nil {
								done <- err
								return
							}
							if err := verifyWorkerContinuation(permit.Authority, ticket, pin, tracker.WorkerPermit, owner.WriteUID, digest); err != nil {
								done <- err
								return
							}
							if lostReply {
								cancelCall()
								done <- nil
								return
							}
							if notSent {
								owner.Phase = db.TrackerWriteNotSent
								owner.Result = []byte(`{"not_sent":true}`)
							} else {
								owner.Phase = db.TrackerWriteConfirmed
								owner.ProviderAcknowledged = true
								owner.Result, _ = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: at})
							}
							reply.Data, _ = json.Marshal(trackerWorkerStatusOutcome{Proof: &owner})
							done <- runtime.workerBroker.reply(project.UID, selection.OwnerInstanceUID, reply)
							return
						}
					}
				}
			}()
			args := trackerWorkerArguments{Notion: &notionsync.Config{DataSourceID: "source", DatabaseID: "parent"}, ID: "item", Target: "closed"}
			observation, err := runtime.WriteNamedStatus(callCtx, binding, joined, "closed", guard, func(call context.Context, admit func() error) (issuesync.StatusObservation, error) {
				return run.writeStatus(call, args, admit)
			}, func(context.Context) error { return nil })
			if notSent || lostReply {
				require.Error(t, err)
				if lostReply {
					require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown)
				}
				require.Empty(t, observation.TrackerWriteUID)
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, observation.TrackerWriteUID)
			}
			require.NoError(t, <-done)
			head, found, err := store.LoadTrackerExecutionFence(ctx, store.InstanceUID(), project.UID, "coordinator")
			require.NoError(t, err)
			require.True(t, found)
			if lostReply {
				require.Equal(t, db.TrackerWriteUnknown, head.Phase)
				require.False(t, head.ProviderAcknowledged)
				pending, err := store.IssueStatusMappingByID(ctx, guard, mapping.ID)
				require.NoError(t, err)
				require.Equal(t, joined.PendingEvent.UID, pending.State.PendingEventUID)
				_, found, err := store.LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), project.UID, selection.OwnerInstanceUID)
				require.NoError(t, err)
				require.False(t, found, "lost reply cannot invent owner settlement")
				return
			}
			if notSent {
				require.Equal(t, db.TrackerWriteNotSent, head.Phase)
				require.False(t, head.ProviderAcknowledged)
				pending, err := store.IssueStatusMappingByID(ctx, guard, mapping.ID)
				require.NoError(t, err)
				require.Equal(t, joined.PendingEvent.UID, pending.State.PendingEventUID)
			} else {
				require.Equal(t, db.TrackerWriteConfirmed, head.Phase)
				_, _, err = store.ObserveIssueStatus(ctx, db.IssueStatusObservationParams{Guard: guard, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: issue.UID, Observation: db.IssueStatusObservation{Version: at}, Status: "closed", ServicedEventUID: joined.PendingEvent.UID, TrackerWriteUID: observation.TrackerWriteUID})
				require.NoError(t, err)
			}
			cp, found, err := store.LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), project.UID, selection.OwnerInstanceUID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, uint64(1), cp.Sequence)
		})
	}
}
