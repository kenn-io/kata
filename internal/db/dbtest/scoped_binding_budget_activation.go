package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunScopedBindingBudgetActivation exercises the native binding accountant at
// C-1/C/C+1 when the third real root checkpoint adds its default signed-body
// delivery to one global stream. Ignoring that offer, double-counting custody,
// or evicting owed bytes to admit it must break this contract. Typed control
// custody is complete-owner retention, not runtime control admission.
func RunScopedBindingBudgetActivation(t *testing.T, probe, root db.Storage, finalDelta int64, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	require.Contains(t, []int64{-1, 0, 1}, finalDelta)
	// No large body exists until a SMALL independent native publication has
	// corroborated the actual header, default envelope and row accountant.
	q := func(n int64) int64 { return int64(base64.StdEncoding.EncodedLen(int(n))) }
	const headerBytes, deliveryFixed, envelopeFixed = int64(524), int64(211), int64(112)
	envelopeSize := func(n int64) int64 { return envelopeFixed + q(headerBytes) + q(n) }
	deliverySize := func(n int64) int64 { return deliveryFixed + headerBytes + envelopeSize(n) }
	offers := func(owner []db.ImportRecord) map[int64]*db.ScopedDeliveryRecord {
		out := map[int64]*db.ScopedDeliveryRecord{}
		for _, record := range owner {
			if row, ok := record.(*db.ScopedDeliveryRecord); ok {
				require.Equal(t, db.ScopedDeliveryOutbound, row.Direction)
				require.Equal(t, db.ScopedDeliveryView, row.Channel)
				_, duplicate := out[row.Sequence]
				require.False(t, duplicate, "there is exactly one binding/epoch/channel stream")
				out[row.Sequence] = row
			}
		}
		return out
	}
	checkOffer := func(row *db.ScopedDeliveryRecord, checkpoint db.ScopedCheckpoint, body []byte, sequence int64, sender string) {
		require.NotNil(t, row)
		require.NoError(t, row.Validate())
		header, decoded, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
		require.NoError(t, err)
		require.True(t, bytes.Equal(body, decoded), "exact native body, with bounded diagnostics")
		transcript, err := db.ScopedDeliveryHeaderBytes(row.EnvelopeBytes)
		require.NoError(t, err)
		require.True(t, bytes.Equal(row.HeaderBytes, transcript))
		require.Equal(t, checkpoint.View.Route[0].BindingUID, row.BindingUID)
		require.Equal(t, checkpoint.View.Route[0].Epoch, row.Epoch)
		require.Equal(t, int64(1), row.Epoch, "one-digit epoch is a measured geometry premise")
		require.Equal(t, sequence, row.Sequence)
		require.Equal(t, sequence, header.Sequence)
		require.Equal(t, checkpoint.View.Key.ViewUID, row.ObjectUID)
		require.Equal(t, checkpoint.Cut.Digest, row.ObjectDigest)
		require.Equal(t, row.ObjectUID, header.ObjectUID)
		require.Equal(t, row.ObjectDigest, header.ObjectDigest)
		require.Equal(t, row.BindingUID, header.BindingUID)
		require.Equal(t, row.Epoch, header.Epoch)
		require.Equal(t, db.ScopedDeliveryView, header.Channel)
		require.Equal(t, checkpoint.View.AuthorityUID, header.AuthorityUID)
		require.Equal(t, sender, header.SenderInstanceUID)
		require.Equal(t, checkpoint.View.Route[0].InstanceUID, header.ReceiverInstanceUID)
		require.Equal(t, []string{sender}, header.Path)
		require.Equal(t, db.ScopedOfferedDigest(body), header.BodyDigest)
		require.Equal(t, db.ScopedOfferedDigest(row.EnvelopeBytes), row.OfferedDigest)
		require.Nil(t, row.Upstream)
		require.False(t, row.Accepted)
		require.False(t, row.Emitted)
		require.False(t, row.Acknowledged)
		require.Equal(t, headerBytes, int64(len(row.HeaderBytes)))
		require.Equal(t, envelopeFixed, int64(len(row.EnvelopeBytes))-q(int64(len(row.HeaderBytes)))-q(int64(len(body))))
		require.Equal(t, deliveryFixed, db.ScopedRetainedBytes(row)-int64(len(row.HeaderBytes))-int64(len(row.EnvelopeBytes)))
		require.Equal(t, envelopeSize(int64(len(body))), int64(len(row.EnvelopeBytes)))
		require.Equal(t, deliverySize(int64(len(body))), db.ScopedRetainedBytes(row))
		require.LessOrEqual(t, len(row.EnvelopeBytes), db.MaxScopedEnvelopeBytes)
	}
	probeFixture := NewScopedViewsFixture(t, probe)
	probeBody, err := json.Marshal(probeFixture.Checkpoint, json.Deterministic(true))
	require.NoError(t, err)
	require.Less(t, len(probeBody), 1<<20)
	probeProducer := probe.(interface {
		PrepareScopedCheckpointBytes(context.Context, []byte) error
		ActivateScopedCheckpoint(context.Context, db.ScopedViewKey, string) error
	})
	probePins := []db.RootKeyPin{}
	for _, selected := range probeFixture.Checkpoint.View.Projects {
		pin, err := probe.RootAuthority(t.Context(), selected.ProjectUID)
		require.NoError(t, err)
		require.Equal(t, selected.Pin, pin)
		require.Equal(t, []byte(probeFixture.Signers[selected.ProjectUID].PrivateKey.Public().(ed25519.PublicKey)), pin.PublicKey)
		probePins = append(probePins, pin)
	}
	require.NoError(t, db.VerifyScopedCheckpoint(probeFixture.Checkpoint, probeFixture.Checkpoint.View, probePins))
	require.NoError(t, probeProducer.PrepareScopedCheckpointBytes(t.Context(), probeBody))
	require.NoError(t, probeProducer.ActivateScopedCheckpoint(t.Context(), probeFixture.Checkpoint.View.Key, probeFixture.Checkpoint.Cut.Digest))
	probeOwner := scopedAuthoritySnapshot(t, probe)
	require.NoError(t, db.ValidateImportRecords(probeOwner))
	probeOffers := offers(probeOwner)
	require.Len(t, probeOffers, 1)
	checkOffer(probeOffers[1], probeFixture.Checkpoint, probeBody, 1, probe.InstanceUID())
	probeIdle := ScopedCaptureWork(t, probe, probeFixture.Checkpoint.View.Key.ViewUID)
	require.Equal(t, "idle", probeIdle.State, "real publication completes its capture CAS")
	require.Nil(t, probeIdle.CaptureUID)
	probeOwner, probeOffers, probeBody = nil, nil, nil

	f := NewScopedViewsFixture(t, root)
	// Existing fixtures reuse deterministic identities in isolated stores.
	// The independent handles and empty target stream below prove separation.
	require.NotSame(t, probe, root, "the small geometry probe is a separate native store")
	checkpoints := []db.ScopedCheckpoint{f.Checkpoint}
	capturer := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	for n := 0; n < 2; n++ {
		params := f.Root.Params
		params.View.Key.ViewUID = scopedOwnerUID(11000 + n*2)
		params.CatalogEntryUID = scopedOwnerUID(11001 + n*2)
		require.NoError(t, params.View.Validate())
		grant, err := root.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), params)
		require.NoError(t, err, "each view is admitted by actual native parent/grant/pin/policy/route checks")
		require.Equal(t, f.Root.Grant.Enrollment.ID, grant.Enrollment.ID)
		capture, err := capturer.CaptureScopedView(t.Context(), params.View.Key)
		require.NoError(t, err)
		projected, err := db.ProjectScopedState(capture, params.View)
		require.NoError(t, err)
		checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: params.View, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: projected, KeyHistory: []db.RootKeyTransition{}}, f.Signers)
		require.NoError(t, err)
		checkpoints = append(checkpoints, checkpoint)
	}
	require.Len(t, checkpoints, 3)
	keys := []db.ScopedViewKey{checkpoints[0].View.Key, checkpoints[1].View.Key, checkpoints[2].View.Key}
	require.NotEqual(t, keys[0].ViewUID, keys[1].ViewUID)
	require.NotEqual(t, keys[0].ViewUID, keys[2].ViewUID)
	require.NotEqual(t, keys[1].ViewUID, keys[2].ViewUID)
	hop := checkpoints[0].View.Route[0]
	compact := make([][]byte, len(checkpoints))
	initialWork := make([]db.ScopedWorkExport, len(checkpoints))
	for n, checkpoint := range checkpoints {
		require.Equal(t, root.InstanceUID(), checkpoint.View.AuthorityUID)
		require.Len(t, checkpoint.View.Route, 1)
		require.Equal(t, hop, checkpoint.View.Route[0])
		require.Equal(t, checkpoints[0].View.RouteDigest, checkpoint.View.RouteDigest)
		require.Equal(t, checkpoints[0].View.Projects, checkpoint.View.Projects)
		pins := []db.RootKeyPin{}
		for _, selected := range checkpoint.View.Projects {
			pin, err := root.RootAuthority(t.Context(), selected.ProjectUID)
			require.NoError(t, err)
			require.Equal(t, selected.Pin, pin)
			require.Equal(t, root.InstanceUID(), pin.AuthorityUID)
			require.Equal(t, []byte(f.Signers[selected.ProjectUID].PrivateKey.Public().(ed25519.PublicKey)), pin.PublicKey)
			pins = append(pins, pin)
		}
		require.NoError(t, db.VerifyScopedCheckpoint(checkpoint, checkpoint.View, pins))
		compact[n], err = json.Marshal(checkpoint, json.Deterministic(true))
		require.NoError(t, err)
		require.Less(t, len(compact[n]), 1<<20)
		initialWork[n] = ScopedCaptureWork(t, root, keys[n].ViewUID)
		require.Equal(t, "captured", initialWork[n].State)
		require.NotNil(t, initialWork[n].CaptureUID)
		require.Equal(t, checkpoint.Cut.CaptureUID, *initialWork[n].CaptureUID)
		require.NotNil(t, initialWork[n].CapturedRevision)
		require.NotNil(t, initialWork[n].CaptureDigest)
		require.Equal(t, db.ScopedOfferedDigest(initialWork[n].CaptureBytes), *initialWork[n].CaptureDigest)
		require.Less(t, db.ScopedRetainedBytes(&initialWork[n]), int64(1<<20))
	}
	// Live checks are SELECT-only. Never recapture an idle view just to prove
	// authority, replace work state, or manufacture room by deleting custody.
	reload := func() db.APIToken {
		parent, err := root.ResolveAPIToken(t.Context(), "parent-test-token")
		require.NoError(t, err)
		allowed, err := root.AccessibleProjectUIDs(t.Context(), parent.Actor)
		require.NoError(t, err)
		ctx := db.WithAuthorizedProjects(t.Context(), allowed)
		for _, selected := range checkpoints[0].View.Projects {
			require.Contains(t, allowed, selected.ProjectUID)
			project, err := root.ProjectByUID(t.Context(), selected.ProjectUID)
			require.NoError(t, err)
			pin, err := root.RootAuthority(t.Context(), project.UID)
			require.NoError(t, err)
			require.Equal(t, selected.Pin, pin)
			// The ONE actual grant is anchored at the original native project;
			// it authorizes the complete selected set, not one grant per peer.
			if project.UID != f.Root.Projects[0].UID {
				continue
			}
			grants, err := root.ListProjectFederationEnrollments(t.Context(), project.ID)
			require.NoError(t, err)
			matched := 0
			for _, grant := range grants {
				if grant.RelayBindingUID == hop.BindingUID {
					matched++
					require.Equal(t, &parent.ID, grant.ParentTokenID)
					require.Equal(t, parent.Actor, grant.Actor)
					require.Equal(t, hop.Epoch, grant.RelayResetEpoch)
					require.Equal(t, hop.InstanceUID, grant.SpokeInstanceUID)
					require.Equal(t, db.ScopedRelayProtocolVersion, grant.RelayProtocolVersion)
					require.Nil(t, grant.RevokedAt)
				}
			}
			require.Equal(t, 1, matched)
		}
		for _, key := range keys {
			require.NoError(t, checkNative(ctx, key), "actual root parent/grant/account/pins/floor/cutover/policy/route checks")
		}
		return parent
	}
	reload()
	owner := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(owner))
	require.Empty(t, offers(owner), "target starts with no hidden probe or per-view stream rows")
	viewCount := 0
	for _, record := range owner {
		if row, ok := record.(*db.ScopedViewExport); ok {
			viewCount++
			require.Equal(t, hop.BindingUID, row.BindingUID)
		}
	}
	require.Equal(t, 3, viewCount)
	// This arithmetic is corroborated by the preceding ACTUAL small offer,
	// before any large runtime allocation. All epoch/sequence widths stay 1.
	bodySizes := []int64{db.MaxScopedCheckpointBytes, db.MaxScopedCheckpointBytes, 17 << 20}
	require.Equal(t, int64(89480035), deliverySize(bodySizes[0]))
	require.Equal(t, int64(23769271), deliverySize(bodySizes[2]))
	for _, size := range bodySizes {
		require.LessOrEqual(t, envelopeSize(size), int64(db.MaxScopedEnvelopeBytes))
	}
	targetControlBytes := int64(db.MaxScopedBindingBytes) + finalDelta
	for _, size := range bodySizes {
		targetControlBytes -= deliverySize(size)
	}
	require.Equal(t, int64(65706115)+finalDelta, targetControlBytes)
	require.Less(t, targetControlBytes+(1<<20), int64(db.MaxScopedRecoveryBytes), "each real sub-1MiB work tuple leaves private recovery headroom")
	owner = nil
	producer := root.(interface {
		PrepareScopedCheckpointBytes(context.Context, []byte) error
		ActivateScopedCheckpoint(context.Context, db.ScopedViewKey, string) error
	})
	generation := func(owner []db.ImportRecord, key db.ScopedViewKey) *db.ScopedGenerationExport {
		for _, record := range owner {
			if row, ok := record.(*db.ScopedGenerationExport); ok && row.ViewUID == key.ViewUID && row.Generation == key.Generation {
				return row
			}
		}
		t.Fatal("actual retained generation is missing")
		return nil
	}
	idleWork := make([]db.ScopedWorkExport, 2)
	for n, checkpoint := range checkpoints {
		// Legal outer whitespace changes no signed facts; the bytes retained
		// here and forwarded by the real native publisher remain exact.
		body := make([]byte, int(bodySizes[n]))
		copy(body, compact[n])
		for i := len(compact[n]); i < len(body); i++ {
			body[i] = ' '
		}
		require.NoError(t, producer.PrepareScopedCheckpointBytes(t.Context(), body))
		prepared := scopedAuthoritySnapshot(t, root)
		require.NoError(t, db.ValidateImportRecords(prepared))
		retained := generation(prepared, keys[n])
		require.True(t, bytes.Equal(body, retained.CheckpointBytes))
		require.Equal(t, bodySizes[n], retained.AssembledBytes)
		require.Equal(t, initialWork[n], ScopedCaptureWork(t, root, keys[n].ViewUID))
		if n < 2 {
			require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), keys[n], checkpoint.Cut.Digest))
			published := scopedAuthoritySnapshot(t, root)
			require.NoError(t, db.ValidateImportRecords(published))
			actual := offers(published)
			require.Len(t, actual, n+1)
			checkOffer(actual[int64(n+1)], checkpoint, body, int64(n+1), root.InstanceUID())
			idleWork[n] = ScopedCaptureWork(t, root, keys[n].ViewUID)
			require.Equal(t, "idle", idleWork[n].State, "only real publication/CAS establishes idle")
			require.Equal(t, *initialWork[n].CapturedRevision, idleWork[n].DirtyRevision)
			require.Nil(t, idleWork[n].CapturedRevision)
			require.Nil(t, idleWork[n].CaptureUID)
			require.Nil(t, idleWork[n].CaptureDigest)
			require.Empty(t, idleWork[n].CaptureBytes)
		}
	}
	parent := reload()
	native, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	principal := db.CustodyPrincipal{AuthorityUID: checkpoints[0].View.AuthorityUID, BindingUID: hop.BindingUID, Epoch: hop.Epoch, AccountableActor: parent.Actor, RouteDigest: checkpoints[0].View.RouteDigest}
	request := db.ScopedControlRequest{RequestUID: scopedOwnerUID(11010), Digest: strings.Repeat("d", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: native.ProjectUID, IssueUID: native.UID}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: root.InstanceUID(), Holder: parent.Actor, ClientKind: "cli"}}
	require.NoError(t, request.Validate())
	control := &db.ScopedControlCustodyExport{RequestUID: request.RequestUID, RequestDigest: request.Digest, BindingUID: hop.BindingUID, PrincipalBytes: scopedOwnerBytes(t, principal), RequestBytes: scopedOwnerBytes(t, request), State: "pending"}
	controlFixed := db.ScopedRetainedBytes(control) - int64(len(control.RequestBytes))
	requestSize := targetControlBytes - controlFixed
	require.Greater(t, requestSize, int64(len(control.RequestBytes)))
	largeRequest := make([]byte, int(requestSize))
	copy(largeRequest, control.RequestBytes)
	for i := len(control.RequestBytes); i < len(largeRequest); i++ {
		largeRequest[i] = ' '
	}
	control.RequestBytes = largeRequest
	require.Equal(t, targetControlBytes, db.ScopedRetainedBytes(control))
	// Only the one valid typed control row is appended to ACTUAL native
	// exports. No view, generation, delivery, work or owner row is staged.
	owner = scopedAuthoritySnapshot(t, root)
	for _, record := range owner {
		switch record.(type) {
		case *db.ScopedControlCustodyExport, *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedConversionExport, *db.ScopedConversionObligationExport:
			t.Fatal("minimal target has no hidden control/source/conversion custody")
		}
	}
	owner = append(owner, control)
	// Separate actual binding, each private work+custody, and daemon totals.
	// Cursor rows are daemon inventory, never another binding charge.
	budget := func(records []db.ImportRecord) (int64, int64) {
		var binding, daemon, payload int64
		works, controls := map[string]int64{}, 0
		for _, record := range records {
			if db.IsScopedImportRecord(record) {
				daemon += db.ScopedRetainedBytes(record)
			}
			switch row := record.(type) {
			case *db.ScopedDeliveryRecord:
				require.Equal(t, hop.BindingUID, row.BindingUID)
				binding += db.ScopedRetainedBytes(row)
				payload += db.ScopedRetainedBytes(row)
			case *db.ScopedControlCustodyExport:
				controls++
				require.Equal(t, request.RequestUID, row.RequestUID)
				require.True(t, reflect.DeepEqual(control, row))
				binding += db.ScopedRetainedBytes(row)
				payload += db.ScopedRetainedBytes(row)
			case *db.ScopedWorkExport:
				works[row.ViewUID] = db.ScopedRetainedBytes(row)
				require.Less(t, works[row.ViewUID], int64(1<<20))
			case *db.ScopedGenerationExport:
				payload += int64(len(row.CheckpointBytes))
			case *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedConversionExport, *db.ScopedConversionObligationExport:
				t.Fatal("this binding construction has no source/conversion contribution")
			}
		}
		require.Equal(t, 1, controls)
		require.Len(t, works, 3)
		for _, key := range keys {
			require.Positive(t, works[key.ViewUID])
			require.Less(t, works[key.ViewUID]+targetControlBytes, int64(db.MaxScopedRecoveryBytes))
		}
		require.Less(t, daemon-payload, int64(8<<20), "measured non-payload scoped inventory is small")
		require.Less(t, daemon, int64(db.MaxScopedDaemonBytes))
		return binding, daemon
	}
	beforeBinding := int64(db.MaxScopedBindingBytes) + finalDelta - deliverySize(bodySizes[2])
	actualBinding, _ := budget(owner)
	require.Equal(t, beforeBinding, actualBinding)
	require.Less(t, actualBinding, int64(db.MaxScopedBindingBytes))
	require.NoError(t, db.ValidateImportRecords(owner), "complete owner at the legal pre-activation boundary")
	require.NoError(t, root.ImportReplay(t.Context(), owner, db.ImportOptions{}))
	recovered := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(recovered), "actual replay/recovery retains the complete graph")
	// The input appended the one new control last; native export puts it in
	// its own collection. Compare EVERY other full row in native order and
	// this exact full control separately, without dropping a sequence/body.
	recoveredWithoutControl := []db.ImportRecord{}
	recoveredControlCount := 0
	for _, record := range recovered {
		if row, ok := record.(*db.ScopedControlCustodyExport); ok {
			recoveredControlCount++
			require.True(t, reflect.DeepEqual(control, row))
			continue
		}
		recoveredWithoutControl = append(recoveredWithoutControl, record)
	}
	require.Equal(t, 1, recoveredControlCount)
	require.True(t, reflect.DeepEqual(owner[:len(owner)-1], recoveredWithoutControl), "complete export/recovery, including EVERY sequence and large body")
	reload()
	gotNative, err := root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, gotNative, "stable UID/native IDs are re-resolved after replay")
	for n := 0; n < 2; n++ {
		require.Equal(t, idleWork[n], ScopedCaptureWork(t, root, keys[n].ViewUID))
	}
	require.Equal(t, initialWork[2], ScopedCaptureWork(t, root, keys[2].ViewUID))
	owner, recovered, recoveredWithoutControl = nil, nil, nil
	sequences := func(records []db.ImportRecord) []db.SequenceExport {
		out := []db.SequenceExport{}
		for _, record := range records {
			if row, ok := record.(*db.SequenceExport); ok {
				out = append(out, *row)
			}
		}
		require.NotEmpty(t, out)
		return out
	}
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	for _, record := range before {
		switch row := record.(type) {
		case *db.ScopedViewExport:
			if row.ViewUID == keys[2].ViewUID {
				require.Nil(t, row.ActiveGeneration)
				require.Equal(t, &keys[2].Generation, row.PreparedGeneration)
			}
		case *db.ScopedProjectExport:
			require.Nil(t, row.OwnerViewUID, "outgoing root projects stay canonical")
			require.Nil(t, row.OwnerGeneration)
			require.False(t, row.ServingFenced)
		}
	}
	actualBinding, _ = budget(before)
	require.Equal(t, beforeBinding, actualBinding)
	cursor := scopedViewsCursor(t, root)
	// A genuine short-circuit exact-body retry at the existing prepared key
	// is a prerequisite, not the fresh activation capacity target.
	thirdBody := []byte(generation(before, keys[2]).CheckpointBytes)
	retryErr := producer.PrepareScopedCheckpointBytes(t.Context(), thirdBody)
	afterRetry := scopedAuthoritySnapshot(t, root)
	require.Equal(t, sequences(before), sequences(afterRetry), "EVERY sequence before inspecting retry result")
	require.True(t, reflect.DeepEqual(before, afterRetry), "whole owner remains exact on byte-keyed preparation retry")
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	require.NoError(t, retryErr)
	afterRetry = nil
	activateErr := producer.ActivateScopedCheckpoint(t.Context(), keys[2], checkpoints[2].Cut.Digest)
	after := scopedAuthoritySnapshot(t, root)
	// No native entity/event allocator should run on this outgoing path,
	// including a refused publication. No sequence masks or exceptions.
	require.Equal(t, sequences(before), sequences(after), "EVERY sequence FIRST, before any refusal assertion")
	if finalDelta > 0 {
		require.True(t, reflect.DeepEqual(before, after), "ENTIRE owner, raw bytes, tuples, prefix, pins and custody roll back exactly")
	}
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	gotNative, err = root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, gotNative)
	for n := 0; n < 2; n++ {
		require.Equal(t, idleWork[n], ScopedCaptureWork(t, root, keys[n].ViewUID))
	}
	// Outside the third view's permitted activation/CAS and its new offer,
	// preserve full native/scoped/control/pins/cursor rows on success too.
	unchanged := func(records []db.ImportRecord) []db.ImportRecord {
		out := []db.ImportRecord{}
		for _, record := range records {
			switch row := record.(type) {
			case *db.ScopedViewExport:
				if row.ViewUID == keys[2].ViewUID {
					continue
				}
			case *db.ScopedGenerationExport:
				if row.ViewUID == keys[2].ViewUID {
					continue
				}
			case *db.ScopedWorkExport:
				if row.ViewUID == keys[2].ViewUID {
					continue
				}
			case *db.ScopedDeliveryRecord:
				if row.Sequence == 3 {
					continue
				}
			}
			out = append(out, record)
		}
		return out
	}
	require.True(t, reflect.DeepEqual(unchanged(before), unchanged(after)), "every unaffected full row/body remains exact; no eviction")
	require.NoError(t, db.ValidateImportRecords(after))
	reload()
	actualBinding, _ = budget(after)
	if finalDelta > 0 {
		require.Equal(t, beforeBinding, actualBinding)
		require.Len(t, offers(after), 2)
		require.Equal(t, initialWork[2], ScopedCaptureWork(t, root, keys[2].ViewUID))
		require.ErrorContains(t, activateErr, "scoped binding capacity exceeded", "only the binding C+1 admission is the target")
		return
	}
	require.NoError(t, activateErr, "fresh C-1/C activation must retain owed custody and add the actual third offer")
	require.Equal(t, int64(db.MaxScopedBindingBytes)+finalDelta, actualBinding)
	finalOffers := offers(after)
	require.Len(t, finalOffers, 3)
	checkOffer(finalOffers[3], checkpoints[2], thirdBody, 3, root.InstanceUID())
	thirdGeneration := generation(after, keys[2])
	require.Equal(t, "active", thirdGeneration.State)
	require.True(t, bytes.Equal(thirdBody, thirdGeneration.CheckpointBytes))
	activeViewCount := 0
	for _, record := range after {
		if row, ok := record.(*db.ScopedViewExport); ok && row.ViewUID == keys[2].ViewUID {
			activeViewCount++
			require.Equal(t, "active", row.State)
			require.Equal(t, &keys[2].Generation, row.ActiveGeneration)
			require.Nil(t, row.PreparedGeneration)
		}
	}
	require.Equal(t, 1, activeViewCount)
	completed := ScopedCaptureWork(t, root, keys[2].ViewUID)
	require.Equal(t, "idle", completed.State)
	require.Equal(t, *initialWork[2].CapturedRevision, completed.DirtyRevision)
	require.Nil(t, completed.CapturedRevision)
	require.Nil(t, completed.CaptureUID)
	require.Nil(t, completed.CaptureDigest)
	require.Empty(t, completed.CaptureBytes)
}
