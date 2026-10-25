package dbtest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// ScopedOwnerFixture stages all twenty owner-backup families through typed
// native import, without raw SQL or runtime acceptance/admission shortcuts.
// Its signed readiness bytes are opaque to PR02; T13 supplies inventory semantics.
// This retention-only owner stays fenced: its native issue is not represented
// by the signed active inventory and must never become verified served content.
type ScopedOwnerFixture struct {
	Records     []db.ImportRecord
	View        db.ScopedViewDescriptor
	Active      db.ScopedViewDescriptor
	Principal   db.CustodyPrincipal
	Operation   db.SourceOperation
	Coverage    db.SourceCoverage
	Work        *db.ScopedWorkExport
	Control     *db.ScopedControlCustodyExport
	Conversion  *db.ScopedConversionExport
	Obligations []*db.ScopedConversionObligationExport
}

func scopedOwnerUID(n int) string { return fmt.Sprintf("%026d", n) }
func scopedOwnerBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}
func scopedOwnerPtr[T any](v T) *T { return &v }
func ScopedOwnerBackupFixture(t *testing.T) ScopedOwnerFixture {
	t.Helper()
	id := scopedOwnerUID
	stamp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p, q, i, peer := id(1), id(2), id(40), id(41)
	authority, binding, instance := id(9), id(7), id(8)
	principal := db.CustodyPrincipal{AuthorityUID: authority, BindingUID: binding, Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	principalRaw := scopedOwnerBytes(t, principal)
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	keys := map[string]ed25519.PrivateKey{}
	for n, project := range []string{p, q} {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(n + 1)}, 32))
		keys[project] = key
		public := key.Public().(ed25519.PublicKey)
		pins = append(pins, db.RootKeyPin{ProjectUID: project, AuthorityUID: authority, KeyID: db.RootPublicKeyID(public), PublicKey: public})
	}
	sign := func(raw []byte, selected []db.RootKeyPin) []db.ScopedSignature {
		out := []db.ScopedSignature{}
		for _, pin := range selected {
			out = append(out, db.ScopedSignature{ProjectUID: pin.ProjectUID, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(keys[pin.ProjectUID], raw))})
		}
		return out
	}
	projectsRaw := scopedOwnerBytes(t, []string{p, q})
	readiness, suspension := []byte(" [ ] \n"), []byte("[]\n")
	cutover := db.ScopedProtocolCutoverCertificate{Version: 1, Cutover: db.ScopedProtocolCutoverExpectation{UID: id(60), AuthorityUID: authority, RequestDigest: strings.Repeat("c", 64), ProjectUIDs: []string{p, q}, Floor: 2, ReadinessDigest: db.ScopedOfferedDigest(readiness), SuspendedBindingsDigest: db.ScopedOfferedDigest(suspension)}}
	raw, err := db.ScopedCutoverCertificateSigningBytes(cutover)
	require.NoError(t, err)
	cutover.Signatures = sign(raw, pins)
	view := db.ScopedViewDescriptor{Version: 2, Schema: 1, Key: db.ScopedViewKey{ViewUID: id(20), Generation: 3}, AuthorityUID: authority, DestinationUID: instance, RouteDigest: principal.RouteDigest, RequestDigest: strings.Repeat("3", 64), Route: []db.ScopedHopBinding{{BindingUID: binding, InstanceUID: instance, Epoch: 1}}, RootCutoverUID: cutover.Cutover.UID}
	for _, pin := range pins {
		view.Projects = append(view.Projects, db.ScopedProjectPin{ProjectUID: pin.ProjectUID, Pin: pin})
	}
	active := view
	active.Key.Generation = 2
	active.Projects = append([]db.ScopedProjectPin{}, view.Projects[:1]...)
	active.RequestDigest = strings.Repeat("2", 64)
	retired := active
	retired.Key.Generation = 1
	retired.RequestDigest = strings.Repeat("1", 64)
	records := []db.ImportRecord{
		&db.MetaKV{Key: "instance_uid", Value: instance},
		&db.ProjectExport{ID: 1, UID: p, Name: "example-project", CreatedAt: stamp.Format(time.RFC3339Nano), Metadata: []byte(`{}`), Revision: 1},
		&db.ProjectExport{ID: 2, UID: q, Name: "peer-project", CreatedAt: stamp.Format(time.RFC3339Nano), Metadata: []byte(`{}`), Revision: 1},
		&db.IssueExport{ID: 1, UID: i, ProjectID: 1, ShortID: "0040", Title: "Example", Status: "closed", Author: "member", CreatedAt: stamp.Format(time.RFC3339Nano), UpdatedAt: stamp.Format(time.RFC3339Nano), Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&db.IssueExport{ID: 2, UID: peer, ProjectID: 2, ShortID: "0041", Title: "Peer", Status: "open", Author: "member", CreatedAt: stamp.Format(time.RFC3339Nano), UpdatedAt: stamp.Format(time.RFC3339Nano), Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&pins[0], &pins[1],
		&db.ScopedViewExport{ViewUID: view.Key.ViewUID, CatalogEntryUID: id(6), AuthorityUID: authority, BindingUID: binding, Epoch: 1, AccountableActor: principal.AccountableActor, RouteDigest: principal.RouteDigest, DestinationUID: instance, ActiveGeneration: scopedOwnerPtr(int64(2)), PreparedGeneration: scopedOwnerPtr(int64(3)), State: "active", RequestDigest: view.RequestDigest},
		&db.ScopedProtocolCutoverExport{CutoverUID: cutover.Cutover.UID, RequestDigest: cutover.Cutover.RequestDigest, ProjectUIDsBytes: projectsRaw, Floor: 2, ReadinessBytes: readiness, SuspendedBindingsBytes: suspension, CertificateBytes: scopedOwnerBytes(t, cutover), State: "committed"},
		&db.ScopedProjectExport{ProjectUID: p, OwnerViewUID: scopedOwnerPtr(active.Key.ViewUID), OwnerGeneration: scopedOwnerPtr(active.Key.Generation), ProtocolFloor: 2, RootCutoverUID: scopedOwnerPtr(cutover.Cutover.UID), ServingFenced: true},
		&db.ScopedProjectExport{ProjectUID: q, ProtocolFloor: 2, RootCutoverUID: scopedOwnerPtr(cutover.Cutover.UID)},
	}
	var activeCut db.ScopedCut
	for n, descriptor := range []db.ScopedViewDescriptor{retired, active, view} {
		gen := &db.ScopedGenerationExport{ViewUID: descriptor.Key.ViewUID, Generation: descriptor.Key.Generation, DescriptorBytes: scopedOwnerBytes(t, descriptor), State: []string{"retired", "active", "prepared"}[n], RootCutoverUID: cutover.Cutover.UID}
		gen.DescriptorDigest = db.ScopedOfferedDigest(gen.DescriptorBytes)
		if n < 2 {
			checkpoint := db.ScopedCheckpoint{View: descriptor, Cut: db.ScopedCut{Sequence: 1, CaptureUID: id(70 + n)}, Collections: db.ScopedCollections{Projects: []db.ScopedProjectState{}, Issues: []db.ScopedIssueState{}, Comments: []db.ScopedCommentState{}, Labels: []db.ScopedLabelState{}, Definitions: []db.ScopedDefinitionState{}, References: []db.ScopedReference{}, Provenance: []db.ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}}
			checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
			require.NoError(t, err)
			raw, err = db.ScopedCheckpointSigningBytes(checkpoint)
			require.NoError(t, err)
			checkpoint.Signatures = sign(raw, pins[:1])
			gen.CheckpointBytes = scopedOwnerBytes(t, checkpoint)
			gen.CheckpointDigest = scopedOwnerPtr(checkpoint.Cut.Digest)
			gen.CutSequence = 1
			gen.CutDigest = checkpoint.Cut.Digest
			gen.AssembledBytes = int64(len(gen.CheckpointBytes))
			if n == 1 {
				activeCut = checkpoint.Cut
			}
		}
		records = append(records, gen)
	}
	capture := db.CanonicalViewState{CaptureUID: id(72), ProjectCuts: map[string]int64{p: 5}, Projects: []db.Project{{UID: p, Name: "example-project", Revision: 1, CreatedAt: stamp}}}
	captureRaw, err := db.EncodeScopedCanonicalCapture(capture)
	require.NoError(t, err)
	work := &db.ScopedWorkExport{ViewUID: view.Key.ViewUID, DirtyRevision: 8, CapturedRevision: scopedOwnerPtr(int64(5)), CaptureUID: scopedOwnerPtr(capture.CaptureUID), CaptureBytes: captureRaw, CaptureDigest: scopedOwnerPtr(db.ScopedOfferedDigest(captureRaw)), State: "captured"}
	records = append(records, work)
	effect := db.ScopedEffect{Collection: "projects", Key: p, Action: "replace", Revision: 2, Project: &db.ScopedProjectState{UID: p, Name: "example-project"}}
	effectRaw := append(scopedOwnerBytes(t, effect), ' ')
	records = append(records, &db.ScopedRecordExport{ViewUID: active.Key.ViewUID, Generation: active.Key.Generation, Collection: effect.Collection, RecordKey: effect.Key, ProjectUID: p, VisibleRevision: 2, Present: true, RecordBytes: effectRaw, Digest: db.ScopedOfferedDigest(effectRaw)})
	batch := db.ScopedBatch{View: active, Cut: db.ScopedCut{Sequence: 2, CaptureUID: capture.CaptureUID}, PreviousDigest: activeCut.Digest, Effects: []db.ScopedEffect{effect}}
	batch.ComponentDigests, err = db.ScopedBatchComponentDigests(batch.Effects)
	require.NoError(t, err)
	batch.Cut.Digest, err = db.ScopedBatchDigest(batch)
	require.NoError(t, err)
	raw, err = db.ScopedBatchSigningBytes(batch)
	require.NoError(t, err)
	batch.Signatures = sign(raw, pins[:1])
	records = append(records, &db.ScopedBatchExport{ViewUID: active.Key.ViewUID, Generation: 2, Sequence: 2, Digest: batch.Cut.Digest, PreviousDigest: batch.PreviousDigest, CaptureUID: capture.CaptureUID, BatchBytes: scopedOwnerBytes(t, batch)})
	proof := db.ScopedMembershipProof{Kind: "root_membership", Endpoint: db.ScopedEndpoint{ProjectUID: p, IssueUID: i}}
	raw, err = db.ScopedMembershipSigningBytes(proof, principal)
	require.NoError(t, err)
	proof.Signatures = sign(raw, pins[:1])
	event := db.RemoteEvent{EventUID: id(52), OriginInstanceUID: instance, ProjectUID: p, Type: "project.metadata_updated", Actor: "member", Payload: []byte("{ \n \"revision_new\" : 1, \"diff\" : { \"z\" : { \"from\" : null, \"to\" : 1 }, \"a\" : { \"from\" : null, \"to\" : 2 } } } "), CreatedAt: stamp}
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	native, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	native = append(append([]byte(" \n"), native...), []byte("\n ")...)
	op := db.SourceOperation{Version: 1, UID: id(51), SourceInstanceUID: instance, CommandKind: "patchProjectMetadata", Principal: principal, Members: []db.SourceMember{{ProjectUID: p, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: 0}}, Membership: []db.ScopedMembershipProof{proof}}
	op.Digest, err = db.SourceOperationDigest(op)
	require.NoError(t, err)
	manifest, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: op, Originals: []db.RemoteEvent{event}, OriginalBytes: [][]byte{native}})
	require.NoError(t, err)
	manifest = append(append([]byte(" \n"), manifest...), []byte("\n ")...)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: p, EventUID: event.EventUID, ContentHash: event.ContentHash, AuthorityUID: authority, AccountableActor: "member", SourceActor: "member", IngressInstanceUID: instance, AcceptedAt: stamp, ResetEpoch: 1, Sequence: 1, KeyID: pins[0].KeyID}, keys[p])
	require.NoError(t, err)
	result := db.SourceResult{OperationUID: op.UID, OperationDigest: op.Digest, State: "accepted", Members: op.Members, Receipts: []db.AttributionReceipt{receipt}}
	raw, err = db.ScopedSourceResultSigningBytes(result)
	require.NoError(t, err)
	result.Signatures = sign(raw, pins[:1])
	records = append(records,
		&db.ScopedSourceOperationExport{OperationUID: op.UID, Digest: op.Digest, SourceInstanceUID: instance, CommandKind: op.CommandKind, ManifestBytes: manifest, PrincipalBytes: principalRaw, PrincipalKey: principalKey, State: "accepted", OverlayState: "settled", AdmissionBytes: scopedOwnerBytes(t, db.ScopedAdmissionEvidence{Version: 1, OperationUID: op.UID, OperationDigest: op.Digest, Principal: principal}), TerminalBytes: scopedOwnerBytes(t, result), CancelRequested: true, RequestFingerprint: db.ScopedOfferedDigest(manifest)},
		&db.ScopedPrivateSourceExport{EventUID: event.EventUID, ContentHash: event.ContentHash, ProjectUID: p, OperationUID: scopedOwnerPtr(op.UID), Ordinal: scopedOwnerPtr(int64(0)), EventBytes: native, ProvenanceBytes: append(scopedOwnerBytes(t, receipt), ' ')},
		&db.ScopedSourceCustodyExport{OperationUID: op.UID, BindingUID: binding, Epoch: 1, Direction: "inbound", PrincipalBytes: principalRaw, ManifestDigest: op.Digest, OwnsComplete: true, InvalidationState: "none"},
		&db.ScopedSourceDependencyExport{OperationUID: op.UID, ProjectUID: p, IssueUID: i, ProofBytes: scopedOwnerBytes(t, proof), State: "satisfied"},
	)
	coverage := db.SourceCoverage{OperationUID: op.UID, OperationDigest: op.Digest, View: active.Key, Cut: batch.Cut, Outcome: "represented"}
	raw, err = db.ScopedSourceCoverageSigningBytes(coverage)
	require.NoError(t, err)
	coverage.Signatures = sign(raw, pins[:1])
	records = append(records, &db.ScopedSourceCoverageExport{OperationUID: op.UID, ViewUID: active.Key.ViewUID, Generation: 2, CutDigest: batch.Cut.Digest, Outcome: coverage.Outcome, CertificateBytes: scopedOwnerBytes(t, coverage)})
	controlRequest := db.ScopedControlRequest{RequestUID: id(50), Digest: strings.Repeat("d", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: p, IssueUID: i}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: instance, Holder: "member", ClientKind: "cli"}}
	control := &db.ScopedControlCustodyExport{RequestUID: controlRequest.RequestUID, BindingUID: binding, RequestDigest: controlRequest.Digest, PrincipalBytes: principalRaw, RequestBytes: scopedOwnerBytes(t, controlRequest), ResultBytes: scopedOwnerBytes(t, db.ScopedControlResult{RequestUID: controlRequest.RequestUID, Digest: controlRequest.Digest, State: "pending"}), State: "pending"}
	records = append(records, control)
	preparation := db.ScopedMutationPreparation{Kind: "patchIssueMetadata", Subject: db.ScopedMutationSubject{Kind: "issue", ProjectUID: p, IssueUID: i}, Predicate: "metadata", MetadataGuard: &db.ScopedMetadataPredicate{Key: "example", IfAbsent: true}}
	predicate := db.ScopedMutationPredicate{Version: 1, Preparation: preparation, Incarnation: 1, CanonicalDigest: strings.Repeat("1", 64)}
	records = append(records, &db.ScopedMutationExpectationExport{TokenDigest: db.ScopedOfferedDigest([]byte("opaque-preparation-token")), ProjectUID: p, SubjectUID: i, PredicateKind: "issue.metadata", PredicateBytes: scopedOwnerBytes(t, predicate), PrincipalBytes: principalRaw, RouteDigest: principal.RouteDigest, Nonce: "nonce", ExpiresAt: stamp.Format(time.RFC3339Nano)}, &db.ScopedPredicateIncarnationExport{ProjectUID: p, SubjectUID: i, PredicateKind: "issue.metadata", Incarnation: 2, CanonicalDigest: strings.Repeat("2", 64)}, &db.ScopedCanonicalCloseExport{IssueUID: i, ProjectUID: p, StatusIntentUID: scopedOwnerPtr(id(53)), StatusClockBytes: scopedOwnerCloseClock(t, db.FoldClock{EventUID: id(53), OriginInstanceUID: instance, HLCPhysicalMS: 1}), ValueBytes: scopedOwnerBytes(t, db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: q, PeerIssueUID: peer})})
	conversionRequest := db.ScopedConversionRequest{UID: id(61), Digest: strings.Repeat("e", 64), CatalogEntryUID: id(6), RouteDigest: principal.RouteDigest, ProjectUIDs: []string{p, q}, Target: view, SuspendBindingUIDs: []string{}}
	conversion := &db.ScopedConversionExport{ConversionUID: conversionRequest.UID, RequestDigest: conversionRequest.Digest, RequestBytes: scopedOwnerBytes(t, conversionRequest), OldBindingsBytes: []byte(`[]`), Phase: "prepare"}
	records = append(records, conversion)
	header := db.ScopedDeliveryHeader{Version: 2, Domain: db.ScopedDeliveryDomain, BindingUID: binding, Epoch: 1, Channel: db.ScopedDeliveryCoverage, Sequence: 1, AuthorityUID: authority, SenderInstanceUID: authority, ReceiverInstanceUID: instance, Path: []string{authority}, ObjectUID: op.UID}
	header.ObjectDigest, err = db.ScopedSourceCoverageObjectDigest(coverage)
	require.NoError(t, err)
	offered, err := db.EncodeScopedDelivery(header, scopedOwnerBytes(t, coverage))
	require.NoError(t, err)
	headerBytes, err := db.ScopedDeliveryHeaderBytes(offered)
	require.NoError(t, err)
	stream := db.ScopedDeliveryStream{BindingUID: binding, Epoch: 1, Direction: db.ScopedDeliveryInbound, Channel: db.ScopedDeliveryCoverage}
	records = append(records, &db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: 1}, ObjectUID: op.UID, ObjectDigest: header.ObjectDigest, OfferedDigest: db.ScopedOfferedDigest(offered), HeaderBytes: headerBytes, EnvelopeBytes: offered, Accepted: true}, &db.ScopedDeliveryCursor{ScopedDeliveryStream: stream, ReceivedSequence: 1, ReceivedDigest: db.ScopedOfferedDigest(offered), AcceptedSequence: 1, AcceptedDigest: db.ScopedOfferedDigest(offered)})
	controlDigest, err := db.ScopedControlRequestObjectDigest(controlRequest)
	require.NoError(t, err)
	obligation := &db.ScopedConversionObligationExport{ConversionUID: conversion.ConversionUID, ObligationUID: id(62), Kind: "control", BindingUID: binding, ObjectUID: control.RequestUID, Digest: controlDigest, Epoch: 1, PrincipalKey: principalKey, State: "pending"}
	records = append(records, obligation)
	return ScopedOwnerFixture{Records: records, View: view, Active: active, Principal: principal, Operation: op, Coverage: coverage, Work: work, Control: control, Conversion: conversion, Obligations: []*db.ScopedConversionObligationExport{obligation}}
}

// ScopedOwnerRootIdleFixture starts native first-capture tests with no captured
// bytes at all. The local instance is the canonical authority; serving native
// grant/fence setup remains the caller's real-API duty, never fixture SQL.
func ScopedOwnerRootIdleFixture(t *testing.T) ScopedOwnerFixture {
	t.Helper()
	f := ScopedOwnerBackupFixture(t)
	records := make([]db.ImportRecord, 0, len(f.Records))
	for _, record := range f.Records {
		switch row := record.(type) {
		case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
			// Leaf-local transport custody does not move with the root fixture.
			continue
		case *db.MetaKV:
			if row.Key == "instance_uid" {
				row.Value = f.Principal.AuthorityUID
			}
		case *db.ScopedProjectExport:
			row.OwnerViewUID = nil
			row.OwnerGeneration = nil
			row.ServingFenced = false
		}
		records = append(records, record)
	}
	f.Records = records
	f.Work.CapturedRevision = nil
	f.Work.CaptureUID = nil
	f.Work.CaptureBytes = nil
	f.Work.CaptureDigest = nil
	f.Work.State = "idle"
	return f
}

func scopedOwnerCloseClock(t *testing.T, clock db.FoldClock) []byte {
	t.Helper()
	raw, err := db.EncodeScopedCloseClockProof(db.ScopedCloseClockProof{Version: 1, StatusClock: clock, CloseBoundaryClock: &clock})
	require.NoError(t, err)
	return raw
}
