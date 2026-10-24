package db

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These are private codec/provenance contracts using unit signing keys. They
// establish no native import execution, stored acceptance or purge behavior.
type scopedImportOutputFixture struct {
	view      ScopedViewDescriptor
	pins      []RootKeyPin
	op        SourceOperation
	event     RemoteEvent
	body      []byte
	receipt   AttributionReceipt
	execution scopedImportExecution
	seal      func(*testing.T, scopedImportExecution) ScopedSourceOperationExport
	row       ScopedSourceOperationExport
}

func newScopedImportOutputFixture(t *testing.T) scopedImportOutputFixture {
	t.Helper()
	view, pins, keys := verificationView(t)
	project := pins[0].ProjectUID
	item := ScopedImportItem{ExternalID: "external-key", Title: "after", Author: "member", Status: "open", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.UnixMilli(1234).UTC()}
	op := SourceOperation{Version: 3, UID: verificationID(61), SourceInstanceUID: view.DestinationUID, CommandKind: "importIssues", Principal: CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: "member", RouteDigest: view.RouteDigest}, ImportIntent: &ScopedImportIntent{ProjectUID: project, Source: "example-import", Items: []ScopedImportItem{item}}}
	var err error
	op.Digest, err = SourceOperationDigest(op)
	require.NoError(t, err)
	wire, err := EncodeScopedSourceSubmission(SourceSubmission{Operation: op})
	require.NoError(t, err)
	principal, err := json.Marshal(op.Principal)
	require.NoError(t, err)
	principalKey, err := ScopedPrincipalKey(op.Principal)
	require.NoError(t, err)
	issue := verificationID(62)
	event := RemoteEvent{EventUID: verificationID(63), OriginInstanceUID: view.AuthorityUID, ProjectUID: project, ProjectName: "example-project", IssueUID: &issue, Type: "issue.updated", Actor: "member", HLCPhysicalMS: 1234, HLCCounter: 2, CreatedAt: time.UnixMilli(1234).UTC(), Payload: []byte(`{"source":"example-import","external_id":"external-key","updated_at":"1970-01-01T00:00:01.234Z","title":"after","old_title":"before"}`)}
	event.ContentHash, err = RemoteEventContentHash(event)
	require.NoError(t, err)
	body, err := EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	receipt, err := SignRootReceipt(AttributionReceipt{Version: 1, ProjectUID: project, EventUID: event.EventUID, ContentHash: event.ContentHash, AuthorityUID: view.AuthorityUID, AccountableActor: "member", SourceActor: event.Actor, IngressInstanceUID: view.AuthorityUID, AcceptedAt: time.Unix(2, 0).UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pins[0].KeyID}, keys[project])
	require.NoError(t, err)
	execution := scopedImportExecution{Version: 1, ProjectUID: project, Salt: bytes.Repeat([]byte{7}, 32), Outputs: []scopedImportNativeOutput{{EventBytes: body, Receipt: receipt}}, MappingEffects: []scopedImportMappingEffect{}}
	seal := func(t *testing.T, execution scopedImportExecution) ScopedSourceOperationExport {
		t.Helper()
		result := SourceResult{OperationUID: op.UID, OperationDigest: op.Digest, State: "accepted"}
		commitment, err := scopedImportExecutionCommitment(result, execution)
		require.NoError(t, err)
		result.ImportOutcome = &ScopedImportOutcome{Commitment: commitment}
		signing, err := ScopedSourceResultSigningBytes(result)
		require.NoError(t, err)
		result.Signatures = verificationSign(signing, pins[:1], keys)
		raw, err := json.Marshal(scopedImportTerminal{Version: 1, Result: &result, Execution: &execution})
		require.NoError(t, err)
		return ScopedSourceOperationExport{OperationUID: op.UID, Digest: op.Digest, SourceInstanceUID: op.SourceInstanceUID, CommandKind: op.CommandKind, ManifestBytes: wire, PrincipalBytes: principal, PrincipalKey: principalKey, State: "accepted", OverlayState: "pending", TerminalBytes: raw, RequestFingerprint: ScopedOfferedDigest(wire)}
	}
	return scopedImportOutputFixture{view, pins, op, event, body, receipt, execution, seal, seal(t, execution)}
}

func TestScopedImportTerminalOutputs(t *testing.T) {
	f := newScopedImportOutputFixture(t)
	view, pins, op, event, body, receipt := f.view, f.pins, f.op, f.event, f.body, f.receipt
	execution, seal, row := f.execution, f.seal, f.row
	var err error
	t.Run("exact-root-output", func(t *testing.T) {
		outputs, err := VerifyScopedImportTerminalOutputs(row, pins, view.AuthorityUID)
		require.NoError(t, err)
		require.Len(t, outputs, 1)
		require.Equal(t, event, outputs[0].Event)
		require.Equal(t, body, outputs[0].EventBytes)
		require.Equal(t, receipt, outputs[0].Receipt)
		require.Empty(t, op.Members, "root outputs never become producer members")
	})
	t.Run("envelope_relative_close_writer", func(t *testing.T) {
		_, _, keys := verificationView(t)
		for _, future := range []bool{false, true} {
			name := "valid"
			if future {
				name = "future"
			}
			t.Run(name, func(t *testing.T) {
				output := event
				output.Type = "issue.snapshot"
				clock := FoldClock{EventUID: verificationID(64), OriginInstanceUID: output.OriginInstanceUID, HLCPhysicalMS: output.HLCPhysicalMS - 1}
				if future {
					clock.HLCPhysicalMS = output.HLCPhysicalMS + 1
				}
				proof := CanonicalCloseState{IssueUID: *output.IssueUID, StatusIntentUID: clock.EventUID, StatusClock: clock}
				output.Payload, err = json.Marshal(map[string]any{"uid": *output.IssueUID, "short_id": "0062", "title": "after", "author": "member", "status": "open", "created_at": time.Unix(1, 0).UTC().Format(EventTimestampFormat), "updated_at": output.CreatedAt.Format(EventTimestampFormat), "metadata": map[string]any{}, "close_state": proof})
				require.NoError(t, err)
				require.NoError(t, ValidateFederationEntries(output.Type, output.EventUID, output.Payload), "both variants have a valid typed payload")
				output.ContentHash, err = RemoteEventContentHash(output)
				require.NoError(t, err)
				exact, err := EncodeRelaySourceEvent(output)
				require.NoError(t, err)
				witness := receipt
				witness.ContentHash = output.ContentHash
				witness, err = SignRootReceipt(witness, keys[output.ProjectUID])
				require.NoError(t, err)
				sealed := execution
				sealed.Outputs = []scopedImportNativeOutput{{EventBytes: exact, Receipt: witness}}
				row := seal(t, sealed)
				canonical := &EventExport{ID: 1, UID: output.EventUID, OriginInstanceUID: output.OriginInstanceUID, ProjectID: 1, ProjectName: output.ProjectName, IssueUID: output.IssueUID, Type: output.Type, Actor: output.Actor, Payload: output.Payload, HLCPhysicalMS: output.HLCPhysicalMS, HLCCounter: output.HLCCounter, ContentHash: output.ContentHash, CreatedAt: output.CreatedAt.Format(EventTimestampFormat)}
				if !future {
					require.NoError(t, ValidateFederationEventEntries(output))
					_, err := VerifyScopedImportTerminalOutputs(row, pins, view.AuthorityUID)
					require.NoError(t, err, "a signed native-compatible snapshot is a valid output witness")
					require.NoError(t, validateImportRecord(canonical))
					return
				}
				require.Error(t, ValidateFederationEventEntries(output), "existing native envelope admission already refuses the future writer")
				t.Run("output_reader", func(t *testing.T) {
					_, err := VerifyScopedImportTerminalOutputs(row, pins, view.AuthorityUID)
					require.Error(t, err, "a valid signature cannot excuse a native-invalid output envelope")
				})
				t.Run("owner_event", func(t *testing.T) {
					require.Error(t, validateImportRecord(canonical), "owner event import must enforce the same native envelope contract")
				})
			})
		}
	})
	t.Run("mapping-only-has-no-witness", func(t *testing.T) {
		changed := execution
		changed.Outputs = []scopedImportNativeOutput{}
		outputs, err := VerifyScopedImportTerminalOutputs(seal(t, changed), pins, view.AuthorityUID)
		require.NoError(t, err)
		require.Empty(t, outputs)
	})
	t.Run("receiving-copy-is-not-root-output-custody", func(t *testing.T) {
		_, err := VerifyScopedImportTerminalOutputs(row, pins, view.DestinationUID)
		require.Error(t, err)
	})
	t.Run("owning-instance-is-required", func(t *testing.T) {
		_, err := VerifyScopedImportTerminalOutputs(row, pins, "")
		require.Error(t, err)
	})
	t.Run("public-certificate-is-not-output-custody", func(t *testing.T) {
		result, err := DecodeScopedSourceTerminalResult(row.TerminalBytes)
		require.NoError(t, err)
		public := row
		public.TerminalBytes, err = json.Marshal(result)
		require.NoError(t, err)
		outputs, err := VerifyScopedImportTerminalOutputs(public, pins, view.DestinationUID)
		require.NoError(t, err)
		require.Empty(t, outputs)
	})
	t.Run("root-cannot-drop-private-execution", func(t *testing.T) {
		result, err := DecodeScopedSourceTerminalResult(row.TerminalBytes)
		require.NoError(t, err)
		stripped := row
		stripped.TerminalBytes, err = json.Marshal(result)
		require.NoError(t, err)
		_, err = VerifyScopedImportTerminalOutputs(stripped, pins, view.AuthorityUID)
		require.Error(t, err, "a valid public certificate cannot replace complete root-private execution custody")
	})
	t.Run("retained-envelope-and-root-signature", func(t *testing.T) {
		for _, mutate := range []func(*ScopedSourceOperationExport){
			func(r *ScopedSourceOperationExport) { r.Digest = op.Digest[:63] + "f" },
			func(r *ScopedSourceOperationExport) { r.State = "canceled" },
			func(r *ScopedSourceOperationExport) { r.PrincipalKey = "other" },
			func(r *ScopedSourceOperationExport) {
				r.ManifestBytes = bytes.Replace(r.ManifestBytes, []byte("example-import"), []byte("changed-import"), 1)
			},
		} {
			changed := row
			mutate(&changed)
			_, err := VerifyScopedImportTerminalOutputs(changed, pins, view.AuthorityUID)
			require.Error(t, err)
		}
		_, err := VerifyScopedImportTerminalOutputs(row, pins[1:], view.AuthorityUID)
		require.Error(t, err, "a commitment without trusted root endorsement is not a witness")
	})
	t.Run("signed-seal-cannot-legitimize-invalid-native-provenance", func(t *testing.T) {
		for _, mutate := range []func(*scopedImportExecution){
			func(e *scopedImportExecution) { e.ProjectUID = pins[1].ProjectUID },
			func(e *scopedImportExecution) { e.Outputs = append(e.Outputs, e.Outputs[0]) },
			func(e *scopedImportExecution) { e.Outputs[0].Receipt.EventUID = verificationID(64) },
			func(e *scopedImportExecution) { e.Outputs[0].Receipt.Signature = bytes.Repeat([]byte{0}, 64) },
			func(e *scopedImportExecution) {
				changed := event
				changed.OriginInstanceUID = view.DestinationUID
				changed.ContentHash, err = RemoteEventContentHash(changed)
				require.NoError(t, err)
				e.Outputs[0].EventBytes, err = EncodeRelaySourceEvent(changed)
				require.NoError(t, err)
			},
			func(e *scopedImportExecution) {
				e.Outputs[0].EventBytes = bytes.Replace(body, []byte("payload"), []byte("unrecognized"), 1)
			},
		} {
			changed := execution
			changed.Outputs = append([]scopedImportNativeOutput(nil), execution.Outputs...)
			mutate(&changed)
			_, err := VerifyScopedImportTerminalOutputs(seal(t, changed), pins, view.AuthorityUID)
			require.Error(t, err)
		}
	})
}

// The existing owning graph phase must accept verified root-private outputs
// without requiring their canonical copies, and reject any disagreement when a
// canonical copy is present. This unit context does not claim native GC/replay.
func TestScopedImportTerminalOwnerGraph(t *testing.T) {
	f := newScopedImportOutputFixture(t)
	t.Run("staged-import-requires-target-project", func(t *testing.T) {
		row := f.row
		row.State = "staged"
		row.TerminalBytes = nil
		require.NoError(t, ValidateScopedImportRecord(&row))
		submission, err := DecodeScopedSourceSubmission(row.ManifestBytes)
		require.NoError(t, err)
		require.Empty(t, submission.Operation.Members, "normalized import input has no producer originals")
		g := scopedImportGraph{
			projects:    map[string]*ProjectExport{f.op.ImportIntent.ProjectUID: {ID: 91, UID: f.op.ImportIntent.ProjectUID}},
			operations:  map[string]*ScopedSourceOperationExport{f.op.UID: &row},
			submissions: map[string]SourceSubmission{f.op.UID: submission}, instance: f.view.AuthorityUID,
		}
		require.NoError(t, g.validateOperations(nil), "complete staged import dependency is valid")
		delete(g.projects, f.op.ImportIntent.ProjectUID)
		require.Error(t, g.validateOperations(nil), "staged imports must retain their immutable target project even without members")
	})
	validate := func(t *testing.T, owner string, canonical *EventExport) error {
		t.Helper()
		submission, err := DecodeScopedSourceSubmission(f.row.ManifestBytes)
		require.NoError(t, err)
		g := scopedImportGraph{
			projects:   map[string]*ProjectExport{f.event.ProjectUID: {ID: 91, UID: f.event.ProjectUID}},
			projectIDs: map[int64]string{91: f.event.ProjectUID}, pins: f.pins,
			operations:  map[string]*ScopedSourceOperationExport{f.op.UID: &f.row},
			submissions: map[string]SourceSubmission{f.op.UID: submission}, instance: owner,
		}
		var records []ImportRecord
		if canonical != nil {
			records = append(records, canonical)
		}
		return g.validateOperations(records)
	}
	t.Run("retained-only", func(t *testing.T) {
		require.NoError(t, validate(t, f.view.AuthorityUID, nil))
	})
	t.Run("missing-owning-instance-cannot-downgrade-root-custody", func(t *testing.T) {
		require.Error(t, validate(t, "", nil))
	})
	t.Run("private-frame-is-root-only", func(t *testing.T) {
		require.Error(t, validate(t, f.view.DestinationUID, nil))
	})
	t.Run("root-public-only-is-incomplete-receiver-public-only-is-legal", func(t *testing.T) {
		saved := f.row
		defer func() { f.row = saved }()
		result, err := DecodeScopedSourceTerminalResult(f.row.TerminalBytes)
		require.NoError(t, err)
		f.row.TerminalBytes, err = json.Marshal(result)
		require.NoError(t, err)
		require.Error(t, validate(t, f.view.AuthorityUID, nil), "do not reconstruct lost private execution from public/canonical state")
		require.NoError(t, validate(t, f.view.DestinationUID, nil), "receiving custody owes the public certificate, not root execution")
	})
	canonical := EventExport{ID: 401, UID: f.event.EventUID, OriginInstanceUID: f.event.OriginInstanceUID, ProjectID: 91, ProjectName: f.event.ProjectName, IssueUID: f.event.IssueUID, RelatedIssueUID: f.event.RelatedIssueUID, Type: f.event.Type, Actor: f.event.Actor, Payload: f.event.Payload, HLCPhysicalMS: f.event.HLCPhysicalMS, HLCCounter: f.event.HLCCounter, ContentHash: f.event.ContentHash, CreatedAt: f.event.CreatedAt.Format(EventTimestampFormat)}
	t.Run("same-exact-canonical-original", func(t *testing.T) {
		require.NoError(t, validate(t, f.view.AuthorityUID, &canonical), "local SQL event identity is not part of the portable original")
	})
	t.Run("same-uid-is-not-enough", func(t *testing.T) {
		for _, mutate := range []func(*EventExport){
			func(e *EventExport) { e.OriginInstanceUID = f.view.DestinationUID },
			func(e *EventExport) { e.ProjectID = 92 },
			func(e *EventExport) { e.ContentHash = "changed" },
			func(e *EventExport) { e.HLCPhysicalMS++ },
			func(e *EventExport) { e.HLCCounter++ },
			func(e *EventExport) { e.Payload = []byte(`{"title":"different"}`) },
			func(e *EventExport) { e.Actor = "other-member" },
		} {
			changed := canonical
			mutate(&changed)
			require.Error(t, validate(t, f.view.AuthorityUID, &changed))
		}
	})
}

// Exercise the existing graph's result obligations and actual ACK/tombstone
// delivery phases with a genuinely signed closed private frame and its public
// certificate. These unit records establish no native import execution or GC.
func TestScopedImportTerminalResultObligations(t *testing.T) {
	f := newScopedImportOutputFixture(t)
	submission, err := DecodeScopedSourceSubmission(f.row.ManifestBytes)
	require.NoError(t, err)
	result, err := DecodeScopedSourceTerminalResult(f.row.TerminalBytes)
	require.NoError(t, err)
	require.NoError(t, VerifyScopedSourceResult(result, f.op, f.pins))
	public, err := json.Marshal(result)
	require.NoError(t, err)
	digest, err := ScopedSourceResultObjectDigest(result)
	require.NoError(t, err)
	g := scopedImportGraph{
		projects: map[string]*ProjectExport{f.event.ProjectUID: {ID: 91, UID: f.event.ProjectUID}},
		pins:     f.pins, instance: f.view.AuthorityUID,
		descriptors:   map[ScopedViewKey]ScopedViewDescriptor{f.view.Key: f.view},
		operations:    map[string]*ScopedSourceOperationExport{f.op.UID: &f.row},
		submissions:   map[string]SourceSubmission{f.op.UID: submission},
		principalKeys: map[string]CustodyPrincipal{f.row.PrincipalKey: f.op.Principal},
	}
	require.NoError(t, g.validateOperations(nil), "closed root frame is valid before testing the remaining consumers")
	obligation := ScopedConversionObligationExport{ConversionUID: verificationID(70), ObligationUID: verificationID(71), Kind: "result", BindingUID: f.op.Principal.BindingUID, Epoch: f.op.Principal.Epoch, ObjectUID: f.op.UID, Digest: digest, PrincipalKey: f.row.PrincipalKey, State: "pending"}
	require.NoError(t, ValidateScopedImportRecord(&obligation), "positive obligation must satisfy its persisted row contract")
	t.Run("private-frame-satisfies-public-result-obligation", func(t *testing.T) {
		require.True(t, g.obligationPresent(&obligation))
		changed := obligation
		changed.Digest = ScopedOfferedDigest([]byte("different result"))
		require.False(t, g.obligationPresent(&changed))
	})
	header := ScopedDeliveryHeader{Version: ScopedRelayProtocolVersion, Domain: ScopedDeliveryDomain, BindingUID: f.op.Principal.BindingUID, Epoch: f.op.Principal.Epoch, Channel: ScopedDeliveryResult, Sequence: 1, AuthorityUID: f.view.AuthorityUID, SenderInstanceUID: f.view.AuthorityUID, ReceiverInstanceUID: f.view.DestinationUID, Path: []string{f.view.AuthorityUID}, ObjectUID: f.op.UID, ObjectDigest: digest}
	wire, err := EncodeScopedDelivery(header, public)
	require.NoError(t, err)
	header, body, err := DecodeScopedDelivery(wire)
	require.NoError(t, err)
	require.Equal(t, public, body)
	require.NotEqual(t, []byte(f.row.TerminalBytes), body, "root execution metadata never enters the wire body")
	headerBytes, err := ScopedDeliveryHeaderBytes(wire)
	require.NoError(t, err)
	stream := ScopedDeliveryStream{BindingUID: header.BindingUID, Epoch: header.Epoch, Direction: ScopedDeliveryOutbound, Channel: ScopedDeliveryResult}
	ref := ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: 1}
	row := ScopedDeliveryRecord{ScopedDeliveryRef: ref, ObjectUID: f.op.UID, ObjectDigest: digest, OfferedDigest: ScopedOfferedDigest(wire), HeaderBytes: headerBytes, EnvelopeBytes: wire, Emitted: true, Acknowledged: true}
	require.NoError(t, row.Validate())
	g.deliveries = map[ScopedDeliveryRef]*ScopedDeliveryRecord{ref: &row}
	g.cursors = map[ScopedDeliveryStream]*ScopedDeliveryCursor{stream: {ScopedDeliveryStream: stream, EmittedSequence: 1, EmittedDigest: row.OfferedDigest, AcknowledgedSequence: 1, AcknowledgedDigest: row.OfferedDigest}}
	t.Run("complete-public-offer-and-ack", func(t *testing.T) {
		require.NoError(t, g.validateDelivery())
	})
	t.Run("acked-result-tombstone-retains-public-obligation", func(t *testing.T) {
		row.EnvelopeBytes = nil
		defer func() { row.EnvelopeBytes = wire }()
		require.NoError(t, row.Validate())
		require.True(t, g.retainedDeliveryObject(header))
		require.NoError(t, g.validateDelivery())
	})
	t.Run("private-frame-is-never-a-result-wire-offer", func(t *testing.T) {
		private := header
		private.BodyDigest = ScopedOfferedDigest(f.row.TerminalBytes)
		require.False(t, g.retainedDeliveryObject(private))
		require.Error(t, g.validateDeliveryBody(private, f.row.TerminalBytes))
	})
}
