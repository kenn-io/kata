package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunScopedDaemonBudgetAccountingGeometry characterizes SMALL native offers and
// their complete retained-byte deltas. It neither approaches the daemon quota
// nor establishes a resource gate for a later large boundary experiment.
func RunScopedDaemonBudgetAccountingGeometry(t *testing.T, root db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	// This existing bootstrap imports small pinned root context. New views,
	// grants, captures and publications below all use actual native APIs.
	f := NewScopedViewsFixture(t, root)
	checkpoints := []db.ScopedCheckpoint{f.Checkpoint}
	capturer := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	for n := 0; n < 2; n++ {
		params := f.Root.Params
		params.View.Route = append([]db.ScopedHopBinding{}, params.View.Route...)
		params.View.Key.ViewUID = scopedOwnerUID(11300 + n*2)
		params.CatalogEntryUID = scopedOwnerUID(11301 + n*2)
		if n == 1 {
			params.View.Route[0].BindingUID = scopedOwnerUID(11310)
			params.View.Route[0].InstanceUID = scopedOwnerUID(11311)
			params.View.DestinationUID = params.View.Route[0].InstanceUID
			params.Token = "independent-geometry-transport-token"
		}
		// RouteDigest is the inherited opaque expected context, not a claimed
		// route-hash algorithm or authority. The factory checks the actual hop.
		require.NoError(t, params.View.Validate())
		grant, err := root.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), params)
		require.NoError(t, err, "real parent/policy/pin/route enrollment is a prerequisite")
		if n == 0 {
			require.Equal(t, f.Root.Grant.Enrollment.ID, grant.Enrollment.ID)
		} else {
			require.NotEqual(t, f.Root.Grant.Enrollment.ID, grant.Enrollment.ID)
		}
		capture, err := capturer.CaptureScopedView(t.Context(), params.View.Key)
		require.NoError(t, err)
		projected, err := db.ProjectScopedState(capture, params.View)
		require.NoError(t, err)
		checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: params.View, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: projected, KeyHistory: []db.RootKeyTransition{}}, f.Signers)
		require.NoError(t, err)
		checkpoints = append(checkpoints, checkpoint)
	}
	require.Len(t, checkpoints, 3)
	require.Equal(t, checkpoints[0].View.Route[0], checkpoints[1].View.Route[0])
	require.NotEqual(t, checkpoints[0].View.Route[0].BindingUID, checkpoints[2].View.Route[0].BindingUID)
	require.NotEqual(t, checkpoints[0].View.DestinationUID, checkpoints[2].View.DestinationUID)
	bodies := make([][]byte, len(checkpoints))
	initialWork := make([]db.ScopedWorkExport, len(checkpoints))
	for n, checkpoint := range checkpoints {
		require.Len(t, checkpoint.View.Route, 1)
		require.Equal(t, root.InstanceUID(), checkpoint.View.AuthorityUID)
		pins := []db.RootKeyPin{}
		for _, selected := range checkpoint.View.Projects {
			pin, err := root.RootAuthority(t.Context(), selected.ProjectUID)
			require.NoError(t, err)
			require.Equal(t, selected.Pin, pin)
			require.Equal(t, []byte(f.Signers[selected.ProjectUID].PrivateKey.Public().(ed25519.PublicKey)), pin.PublicKey)
			pins = append(pins, pin)
		}
		require.NoError(t, db.VerifyScopedCheckpoint(checkpoint, checkpoint.View, pins))
		var err error
		bodies[n], err = json.Marshal(checkpoint, json.Deterministic(true))
		require.NoError(t, err)
		require.LessOrEqual(t, len(bodies[n]), 64<<10, "EVERY compact signed body is bounded BEFORE any preparation")
		initialWork[n] = ScopedCaptureWork(t, root, checkpoint.View.Key.ViewUID)
		require.Equal(t, "captured", initialWork[n].State)
		require.NotNil(t, initialWork[n].CapturedRevision)
		require.Equal(t, initialWork[n].DirtyRevision, *initialWork[n].CapturedRevision)
		require.Equal(t, &checkpoint.Cut.CaptureUID, initialWork[n].CaptureUID)
		require.NotNil(t, initialWork[n].CaptureDigest)
		require.Equal(t, db.ScopedOfferedDigest(initialWork[n].CaptureBytes), *initialWork[n].CaptureDigest)
		require.LessOrEqual(t, len(initialWork[n].CaptureBytes), 64<<10)
	}
	reload := func() {
		parent, err := root.ResolveAPIToken(t.Context(), "parent-test-token")
		require.NoError(t, err)
		allowed, err := root.AccessibleProjectUIDs(t.Context(), parent.Actor)
		require.NoError(t, err)
		ctx := db.WithAuthorizedProjects(t.Context(), allowed)
		anchor, err := root.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
		require.NoError(t, err)
		grants, err := root.ListProjectFederationEnrollments(t.Context(), anchor.ID)
		require.NoError(t, err)
		for _, checkpoint := range checkpoints {
			hop := checkpoint.View.Route[0]
			matched := 0
			for _, grant := range grants {
				if grant.RelayBindingUID != hop.BindingUID {
					continue
				}
				matched++
				require.Equal(t, &parent.ID, grant.ParentTokenID)
				require.Equal(t, parent.Actor, grant.Actor)
				require.Equal(t, hop.InstanceUID, grant.SpokeInstanceUID)
				require.Equal(t, hop.Epoch, grant.RelayResetEpoch)
				require.Equal(t, db.ScopedRelayProtocolVersion, grant.RelayProtocolVersion)
				require.Nil(t, grant.RevokedAt)
			}
			require.Equal(t, 1, matched)
			for _, selected := range checkpoint.View.Projects {
				require.Contains(t, allowed, selected.ProjectUID)
			}
			require.NoError(t, checkNative(ctx, checkpoint.View.Key), "actual current root floor/cutover/pins/parent/grant/policy/route checks")
		}
		require.Len(t, grants, 2, "two independently admitted native bindings, not imported grants")
	}
	reload()
	producer := root.(interface {
		PrepareScopedCheckpointBytes(context.Context, []byte) error
		ActivateScopedCheckpoint(context.Context, db.ScopedViewKey, string) error
	})
	// Exact changed rows are removed only after their ENTIRE values have been
	// checked. Every remaining row, including all native/raw/pin/custody rows,
	// retains its full concrete value and order; hashes are not substitutes.
	sequences := func(rows []db.ImportRecord) []db.SequenceExport {
		out := []db.SequenceExport{}
		for _, record := range rows {
			if row, ok := record.(*db.SequenceExport); ok {
				out = append(out, *row)
			}
		}
		return out
	}
	checkDelta := func(before, after, removed, added []db.ImportRecord) {
		require.Equal(t, sequences(before), sequences(after), "EVERY sequence FIRST")
		subtract := func(rows, exact []db.ImportRecord) []db.ImportRecord {
			out := append([]db.ImportRecord{}, rows...)
			for _, want := range exact {
				found := -1
				for i, row := range out {
					if reflect.DeepEqual(row, want) {
						require.Equal(t, -1, found, "unique full changed row")
						found = i
					}
				}
				require.NotEqual(t, -1, found, "entire expected changed row must exist")
				out = append(out[:found], out[found+1:]...)
			}
			return out
		}
		require.True(t, reflect.DeepEqual(subtract(before, removed), subtract(after, added)), "ALL unrelated full rows/raw fields/order remain exact")
	}
	total := func(rows []db.ImportRecord) int64 {
		var result int64
		for _, row := range rows {
			if db.IsScopedImportRecord(row) {
				result += db.ScopedRetainedBytes(row)
			}
		}
		return result
	}
	bindingTotals := func(rows []db.ImportRecord) map[string]int64 {
		out := map[string]int64{}
		for _, record := range rows {
			switch row := record.(type) {
			case *db.ScopedDeliveryRecord:
				out[row.BindingUID] += db.ScopedRetainedBytes(row)
			case *db.ScopedControlCustodyExport, *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedConversionExport, *db.ScopedConversionObligationExport:
				t.Fatal("small native recipe has no artificial source/control/conversion mass")
			}
		}
		return out
	}
	rowsFor := func(owner []db.ImportRecord, key db.ScopedViewKey) (*db.ScopedViewExport, *db.ScopedGenerationExport, *db.ScopedWorkExport) {
		var view *db.ScopedViewExport
		var generation *db.ScopedGenerationExport
		var work *db.ScopedWorkExport
		for _, record := range owner {
			switch row := record.(type) {
			case *db.ScopedViewExport:
				if row.ViewUID == key.ViewUID {
					require.Nil(t, view)
					view = row
				}
			case *db.ScopedGenerationExport:
				if row.ViewUID == key.ViewUID && row.Generation == key.Generation {
					require.Nil(t, generation)
					generation = row
				}
			case *db.ScopedWorkExport:
				if row.ViewUID == key.ViewUID {
					require.Nil(t, work)
					work = row
				}
			}
		}
		require.NotNil(t, view)
		require.NotNil(t, generation)
		require.NotNil(t, work)
		return view, generation, work
	}
	owner := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(owner))
	require.Empty(t, bindingTotals(owner))
	for _, record := range owner {
		switch record.(type) {
		case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
			t.Fatal("no hidden preexisting stream or cursor")
		}
	}
	originalCursor := scopedViewsCursor(t, root)
	seenStreams := map[db.ScopedDeliveryStream]int64{}
	for n, checkpoint := range checkpoints {
		key, body := checkpoint.View.Key, bodies[n]
		require.LessOrEqual(t, len(body), 64<<10, "runtime guard immediately before PrepareBytes")
		oldView, oldGeneration, oldWork := rowsFor(owner, key)
		require.Equal(t, initialWork[n], *oldWork)
		require.Nil(t, oldView.ActiveGeneration)
		require.Equal(t, &key.Generation, oldView.PreparedGeneration)
		require.Equal(t, "prepared", oldView.State)
		require.Equal(t, "prepared", oldGeneration.State)
		require.Nil(t, oldGeneration.CheckpointBytes)
		require.Nil(t, oldGeneration.CheckpointDigest)
		require.Zero(t, oldGeneration.CutSequence)
		require.Empty(t, oldGeneration.CutDigest)
		require.Zero(t, oldGeneration.AssembledBytes)
		require.NoError(t, producer.PrepareScopedCheckpointBytes(t.Context(), body))
		prepared := scopedAuthoritySnapshot(t, root)
		require.Equal(t, sequences(owner), sequences(prepared), "EVERY sequence FIRST after preparation")
		preparedView, preparedGeneration, preparedWork := rowsFor(prepared, key)
		expectedGeneration := *oldGeneration
		expectedGeneration.CheckpointBytes = db.ScopedNullableBytes(bytes.Clone(body))
		expectedGeneration.CheckpointDigest = &checkpoint.Cut.Digest
		expectedGeneration.CutSequence = checkpoint.Cut.Sequence
		expectedGeneration.CutDigest = checkpoint.Cut.Digest
		expectedGeneration.AssembledBytes = int64(len(body))
		require.Equal(t, expectedGeneration, *preparedGeneration)
		require.Equal(t, *oldView, *preparedView)
		require.Equal(t, *oldWork, *preparedWork)
		effects, err := db.ScopedCheckpointRecordEffects(checkpoint)
		require.NoError(t, err)
		expectedRecords := map[string]db.ScopedEffect{}
		for _, effect := range effects {
			expectedRecords[effect.Collection+"/"+effect.Key] = effect
		}
		added := []db.ImportRecord{&expectedGeneration}
		var inventoryBytes int64
		for _, record := range prepared {
			row, ok := record.(*db.ScopedRecordExport)
			if !ok || row.ViewUID != key.ViewUID {
				continue
			}
			effect, exists := expectedRecords[row.Collection+"/"+row.RecordKey]
			require.True(t, exists)
			delete(expectedRecords, row.Collection+"/"+row.RecordKey)
			require.NoError(t, db.ValidateScopedRecordEnvelope(*row))
			var decoded db.ScopedEffect
			require.NoError(t, db.DecodeScopedRecord(row.RecordBytes, &decoded))
			require.Equal(t, effect, decoded, "full actual signed assertion, not digest-only equality")
			project, err := db.ScopedEffectProjectUID(effect)
			require.NoError(t, err)
			subject, peerProject, peer, err := db.ScopedRecordIndexes(effect, nil)
			require.NoError(t, err)
			expected := db.ScopedRecordExport{ViewUID: key.ViewUID, Generation: key.Generation, Collection: effect.Collection, RecordKey: effect.Key, ProjectUID: project, SubjectUID: subject, PeerProjectUID: peerProject, PeerUID: peer, VisibleRevision: effect.Revision, Present: true, RecordBytes: bytes.Clone(row.RecordBytes), Digest: db.ScopedOfferedDigest(row.RecordBytes)}
			require.Equal(t, expected, *row)
			inventoryBytes += db.ScopedRetainedBytes(row)
			added = append(added, &expected)
		}
		require.Empty(t, expectedRecords)
		checkDelta(owner, prepared, []db.ImportRecord{oldGeneration}, added)
		require.Equal(t, int64(len(body))+128, db.ScopedRetainedBytes(preparedGeneration)-db.ScopedRetainedBytes(oldGeneration), "actual two retained 64-byte cut digests")
		require.Equal(t, int64(len(body))+128+inventoryBytes, total(prepared)-total(owner))
		require.Equal(t, bindingTotals(owner), bindingTotals(prepared), "preparation does not charge raw baseline to binding")
		require.Equal(t, originalCursor, scopedViewsCursor(t, root))
		require.NoError(t, db.ValidateImportRecords(prepared))
		reload()
		require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), key, checkpoint.Cut.Digest))
		published := scopedAuthoritySnapshot(t, root)
		require.Equal(t, sequences(prepared), sequences(published), "EVERY sequence FIRST after activation")
		activeView, activeGeneration, idle := rowsFor(published, key)
		expectedView := *preparedView
		expectedView.ActiveGeneration = &key.Generation
		expectedView.PreparedGeneration = nil
		expectedView.State, expectedView.ReasonCode = "active", ""
		expectedView.RequestDigest = checkpoint.View.RequestDigest
		expectedActiveGeneration := *preparedGeneration
		expectedActiveGeneration.State = "active"
		expectedIdle := *preparedWork
		expectedIdle.State = "idle"
		expectedIdle.CapturedRevision, expectedIdle.CaptureUID, expectedIdle.CaptureBytes, expectedIdle.CaptureDigest = nil, nil, nil, nil
		require.Equal(t, expectedView, *activeView)
		require.Equal(t, expectedActiveGeneration, *activeGeneration)
		require.Equal(t, expectedIdle, *idle, "only actual publication CAS establishes idle")
		hop := checkpoint.View.Route[0]
		stream := db.ScopedDeliveryStream{BindingUID: hop.BindingUID, Epoch: hop.Epoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}
		next := seenStreams[stream] + 1
		var delivery *db.ScopedDeliveryRecord
		var cursor *db.ScopedDeliveryCursor
		for _, record := range published {
			switch row := record.(type) {
			case *db.ScopedDeliveryRecord:
				if row.ScopedDeliveryStream == stream && row.Sequence == next {
					require.Nil(t, delivery)
					delivery = row
				}
			case *db.ScopedDeliveryCursor:
				if row.ScopedDeliveryStream == stream {
					require.Nil(t, cursor)
					cursor = row
				}
			}
		}
		require.NotNil(t, delivery)
		require.NotNil(t, cursor)
		require.NoError(t, delivery.Validate())
		header, decodedBody, err := db.DecodeScopedDelivery(delivery.EnvelopeBytes)
		require.NoError(t, err)
		require.True(t, bytes.Equal(body, decodedBody), "full exact root body")
		expectedHeader := db.ScopedDeliveryHeader{Version: db.ScopedRelayProtocolVersion, Domain: db.ScopedDeliveryDomain, BindingUID: hop.BindingUID, Epoch: hop.Epoch, Channel: db.ScopedDeliveryView, Sequence: next, AuthorityUID: root.InstanceUID(), SenderInstanceUID: root.InstanceUID(), ReceiverInstanceUID: hop.InstanceUID, Path: []string{root.InstanceUID()}, ObjectUID: key.ViewUID, ObjectDigest: checkpoint.Cut.Digest, BodyDigest: db.ScopedOfferedDigest(body)}
		require.Equal(t, expectedHeader, header)
		transcript, err := db.ScopedDeliveryHeaderBytes(delivery.EnvelopeBytes)
		require.NoError(t, err)
		envelope, err := db.EncodeScopedDelivery(expectedHeader, body)
		require.NoError(t, err)
		expectedDelivery := db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: next}, ObjectUID: key.ViewUID, ObjectDigest: checkpoint.Cut.Digest, OfferedDigest: db.ScopedOfferedDigest(envelope), HeaderBytes: transcript, EnvelopeBytes: envelope}
		require.Equal(t, expectedDelivery, *delivery, "all flags, upstream, stream identity and raw envelope")
		require.LessOrEqual(t, len(delivery.EnvelopeBytes), 64<<10)
		require.Equal(t, 524, len(delivery.HeaderBytes), "ACTUAL small canonical header premise")
		require.Equal(t, 112+base64.StdEncoding.EncodedLen(524)+base64.StdEncoding.EncodedLen(len(body)), len(delivery.EnvelopeBytes))
		require.Equal(t, int64(211+524+len(delivery.EnvelopeBytes)), db.ScopedRetainedBytes(delivery))
		expectedCursor := db.ScopedDeliveryCursor{ScopedDeliveryStream: stream}
		require.Equal(t, expectedCursor, *cursor, "publication reserves an un-emitted/un-ACKed global cursor")
		require.Equal(t, int64(78), db.ScopedRetainedBytes(cursor))
		added = []db.ImportRecord{&expectedView, &expectedActiveGeneration, &expectedIdle, &expectedDelivery}
		var cursorDelta int64
		if seenStreams[stream] == 0 {
			added = append(added, &expectedCursor)
			cursorDelta = db.ScopedRetainedBytes(cursor)
		}
		checkDelta(prepared, published, []db.ImportRecord{preparedView, preparedGeneration, preparedWork}, added)
		workDrop := db.ScopedRetainedBytes(preparedWork) - db.ScopedRetainedBytes(idle)
		require.Equal(t, int64(len(preparedWork.CaptureBytes)+102), workDrop, "actual revision/UID/digest/capture/state CAS shrink")
		require.Equal(t, int64(-4), db.ScopedRetainedBytes(activeView)+db.ScopedRetainedBytes(activeGeneration)-db.ScopedRetainedBytes(preparedView)-db.ScopedRetainedBytes(preparedGeneration))
		require.Equal(t, db.ScopedRetainedBytes(delivery)+cursorDelta-4-workDrop, total(published)-total(prepared), "COMPLETE actual activation delta")
		priorBindings := bindingTotals(prepared)
		priorBindings[hop.BindingUID] += db.ScopedRetainedBytes(delivery)
		require.Equal(t, priorBindings, bindingTotals(published), "one binding charges the actual new offer only")
		require.Equal(t, originalCursor, scopedViewsCursor(t, root), "outgoing publication does not mutate native/UI recency")
		require.NoError(t, db.ValidateImportRecords(published))
		require.Less(t, total(published), int64(db.MaxScopedDaemonBytes))
		require.Less(t, db.ScopedRetainedBytes(idle), int64(db.MaxScopedRecoveryBytes))
		for _, size := range bindingTotals(published) {
			require.Less(t, size, int64(db.MaxScopedBindingBytes))
		}
		seenStreams[stream] = next
		owner = published
		reload()
	}
	require.Len(t, seenStreams, 2)
	require.Equal(t, int64(2), seenStreams[db.ScopedDeliveryStream{BindingUID: checkpoints[0].View.Route[0].BindingUID, Epoch: 1, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}])
	require.Equal(t, int64(1), seenStreams[db.ScopedDeliveryStream{BindingUID: checkpoints[2].View.Route[0].BindingUID, Epoch: 1, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}])
	require.NoError(t, db.ValidateImportRecords(owner))
}
