package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

// R02A: hashing an unsigned replacement row cannot change its latest signed
// checkpoint/batch fact, revision, disposition, or complete inventory.
func TestScopedImportRecordMatchesLatestSignedProof(t *testing.T) {
	for _, mutation := range []string{"body", "revision", "remove", "missing_record", "extra_record"} {
		t.Run(mutation, func(t *testing.T) {
			f := dbtest.ScopedOwnerBackupFixture(t)
			require.NoError(t, db.ValidateImportRecords(f.Records))
			records := []db.ImportRecord{}
			for _, r := range f.Records {
				row, ok := r.(*db.ScopedRecordExport)
				if !ok {
					records = append(records, r)
					continue
				}
				if mutation == "missing_record" {
					continue
				}
				changed := *row
				var effect db.ScopedEffect
				require.NoError(t, json.Unmarshal(row.RecordBytes, &effect))
				switch mutation {
				case "body":
					effect.Project.Name = "unsigned replacement"
				case "revision":
					effect.Revision++
				case "remove":
					effect.Project = nil
					effect.Action = "remove"
				case "extra_record":
					records = append(records, row)
					effect = db.ScopedEffect{Collection: "issues", Key: row.ProjectUID + ":00000000000000000000000099", Revision: 2, Action: "replace", Issue: &db.ScopedIssueState{ProjectUID: row.ProjectUID, UID: "00000000000000000000000099", Title: "unsigned extra"}}
					changed.Collection = effect.Collection
					changed.RecordKey = effect.Key
					subject := effect.Issue.UID
					changed.SubjectUID = &subject
				}
				raw, err := json.Marshal(effect)
				require.NoError(t, err)
				changed.RecordBytes = raw
				changed.Digest = db.ScopedOfferedDigest(raw)
				changed.VisibleRevision = effect.Revision
				changed.Present = effect.Action == "replace"
				require.NoError(t, db.ValidateScopedImportRecord(&changed), "the negative case must not fail ordinary row validation")
				records = append(records, &changed)
			}
			require.Error(t, db.ValidateImportRecords(records), "unsigned row mutation cannot replace latest signed evidence")
		})
	}
}

// R02C: full and compacted offers must agree on the independently retained
// exact body commitment, not just the canonical object's separate digest.
func TestScopedImportCompactedExactBodyParity(t *testing.T) {
	for _, variant := range []string{"view", "view_batch", "source", "result", "control", "coverage", "conversion"} {
		t.Run(variant, func(t *testing.T) {
			channel := db.ScopedDeliveryChannel(variant)
			if variant == "view_batch" {
				channel = db.ScopedDeliveryView
			}
			f := dbtest.ScopedOwnerBackupFixture(t)
			var objectUID, digest string
			var body []byte
			for _, r := range f.Records {
				if gen, ok := r.(*db.ScopedGenerationExport); ok && channel == db.ScopedDeliveryView && gen.Generation == f.Active.Key.Generation {
					objectUID, digest, body = gen.ViewUID, gen.CutDigest, gen.CheckpointBytes
				}
				if op, ok := r.(*db.ScopedSourceOperationExport); ok {
					if channel == db.ScopedDeliverySource {
						objectUID, digest, body = op.OperationUID, op.Digest, op.ManifestBytes
					}
					if channel == db.ScopedDeliveryResult {
						var result db.SourceResult
						require.NoError(t, json.Unmarshal(op.TerminalBytes, &result))
						var err error
						digest, err = db.ScopedSourceResultObjectDigest(result)
						require.NoError(t, err)
						objectUID, body = op.OperationUID, op.TerminalBytes
					}
				}
			}
			if variant == "view_batch" {
				for _, r := range f.Records {
					if batch, ok := r.(*db.ScopedBatchExport); ok {
						objectUID, digest, body = batch.ViewUID, batch.Digest, batch.BatchBytes
					}
				}
			}
			var err error
			switch channel {
			case db.ScopedDeliveryControl:
				var request db.ScopedControlRequest
				require.NoError(t, json.Unmarshal(f.Control.RequestBytes, &request))
				objectUID = request.RequestUID
				digest, err = db.ScopedControlRequestObjectDigest(request)
				body = f.Control.RequestBytes
			case db.ScopedDeliveryCoverage:
				objectUID = f.Operation.UID
				digest, err = db.ScopedSourceCoverageObjectDigest(f.Coverage)
				body, err = json.Marshal(f.Coverage)
			case db.ScopedDeliveryConversion:
				var request db.ScopedConversionRequest
				require.NoError(t, json.Unmarshal(f.Conversion.RequestBytes, &request))
				objectUID = request.UID
				digest, err = db.ScopedConversionRequestObjectDigest(request)
				body = f.Conversion.RequestBytes
			}
			require.NoError(t, err)
			exactRecords := fixtureInbound(t, f, channel, objectUID, digest, body)
			require.NoError(t, db.ValidateImportRecords(exactRecords), "full exact offer remains its own representation")
			exactRow := exactRecords[len(exactRecords)-2].(*db.ScopedDeliveryRecord)
			exactRow.EnvelopeBytes = nil
			require.NoError(t, db.ValidateImportRecords(exactRecords), "exact independently retained body supports compaction")
			changedBody := append([]byte(" \n"), body...)
			if channel == db.ScopedDeliverySource {
				changedBody = bytes.TrimSpace(body)
			}
			changedRecords := fixtureInbound(t, f, channel, objectUID, digest, changedBody)
			if channel == db.ScopedDeliveryView || channel == db.ScopedDeliveryResult || channel == db.ScopedDeliveryCoverage {
				require.NoError(t, db.ValidateImportRecords(changedRecords), "full independently verified representation retains its own exact outer whitespace")
			} else {
				require.Error(t, db.ValidateImportRecords(changedRecords), "immutable source/control/conversion request must bind exact retained bytes")
			}
			changedRow := changedRecords[len(changedRecords)-2].(*db.ScopedDeliveryRecord)
			changedRow.EnvelopeBytes = nil
			require.Error(t, db.ValidateImportRecords(changedRecords), "compacted header must commit exact independently retained body")
		})
	}
}

// Object signatures and exact body commitments do not authenticate a different
// delivery hop. Mutate only the envelope context and reseal its normal indexes;
// both native importers must reject it before replacing a valid owner snapshot.
func TestScopedImportDeliveryContext(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			for _, direction := range []db.ScopedDeliveryDirection{db.ScopedDeliveryInbound, db.ScopedDeliveryOutbound} {
				t.Run(string(direction), func(t *testing.T) {
					for _, compacted := range []bool{false, true} {
						phase := "full"
						if compacted {
							phase = "compacted"
						}
						t.Run(phase, func(t *testing.T) {
							fixture := func(mutate func(*db.ScopedDeliveryHeader)) []db.ImportRecord {
								f := dbtest.ScopedOwnerBackupFixture(t)
								if direction == db.ScopedDeliveryOutbound {
									f = dbtest.ScopedOwnerRootIdleFixture(t)
								}
								var body []byte
								for _, record := range f.Records {
									if generation, ok := record.(*db.ScopedGenerationExport); ok && generation.Generation == f.Active.Key.Generation {
										body = generation.CheckpointBytes
									}
								}
								require.NotEmpty(t, body)
								records := fixtureInbound(t, f, db.ScopedDeliveryView, f.Active.Key.ViewUID, mustCheckpointDigest(t, body), body)
								row := records[len(records)-2].(*db.ScopedDeliveryRecord)
								cursor := records[len(records)-1].(*db.ScopedDeliveryCursor)
								header, exactBody, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
								require.NoError(t, err)
								require.Equal(t, body, exactBody)
								if mutate != nil {
									mutate(&header)
								}
								wire, err := db.EncodeScopedDelivery(header, body)
								require.NoError(t, err, "context mutation remains a structurally valid envelope")
								row.HeaderBytes, err = db.ScopedDeliveryHeaderBytes(wire)
								require.NoError(t, err)
								row.BindingUID, row.Epoch, row.Direction = header.BindingUID, header.Epoch, direction
								row.OfferedDigest, row.EnvelopeBytes = db.ScopedOfferedDigest(wire), wire
								cursor.ScopedDeliveryStream = row.ScopedDeliveryStream
								if direction == db.ScopedDeliveryInbound {
									cursor.ReceivedDigest, cursor.AcceptedDigest = row.OfferedDigest, row.OfferedDigest
								} else {
									row.Accepted, row.Emitted, row.Acknowledged = false, true, true
									cursor.ReceivedSequence, cursor.ReceivedDigest, cursor.AcceptedSequence, cursor.AcceptedDigest = 0, "", 0, ""
									cursor.EmittedSequence, cursor.EmittedDigest, cursor.AcknowledgedSequence, cursor.AcknowledgedDigest = 1, row.OfferedDigest, 1, row.OfferedDigest
								}
								if compacted {
									row.EnvelopeBytes = nil
								}
								require.NoError(t, row.Validate())
								require.NoError(t, cursor.Validate())
								return records
							}
							valid := fixture(nil)
							require.NoError(t, db.ValidateImportRecords(valid), "exact retained object and hop form the positive baseline")
							restored := artifactBackupStore(t, backend)
							require.NoError(t, restored.ImportReplay(t.Context(), valid, db.ImportOptions{}), "actual native positive restore precedes context negatives")
							for _, mutation := range []string{"receiver", "binding", "epoch", "extra_hop", "authority"} {
								t.Run(mutation, func(t *testing.T) {
									changed := fixture(func(h *db.ScopedDeliveryHeader) {
										switch mutation {
										case "receiver":
											h.ReceiverInstanceUID = "00000000000000000000000099"
										case "binding":
											h.BindingUID = "00000000000000000000000098"
										case "epoch":
											h.Epoch++
										case "extra_hop":
											h.SenderInstanceUID = "00000000000000000000000097"
											h.Path = append(h.Path, h.SenderInstanceUID)
										case "authority":
											h.AuthorityUID, h.SenderInstanceUID = "00000000000000000000000096", "00000000000000000000000096"
											h.Path = []string{h.AuthorityUID}
										}
									})
									t.Run("validation", func(t *testing.T) {
										require.Error(t, db.ValidateImportRecords(changed), "signed object cannot authorize a resealed different delivery context")
									})
									t.Run("restore_atomic", func(t *testing.T) {
										target := artifactBackupStore(t, backend)
										require.NoError(t, target.ImportReplay(t.Context(), valid, db.ImportOptions{}))
										var before bytes.Buffer
										require.NoError(t, Export(t.Context(), target, &before, ExportOptions{IncludeDeleted: true}))
										require.Error(t, target.ImportReplay(t.Context(), changed, db.ImportOptions{}))
										var after bytes.Buffer
										require.NoError(t, Export(t.Context(), target, &after, ExportOptions{IncludeDeleted: true}))
										require.Equal(t, before.Bytes(), after.Bytes(), "rejected context replacement preserves the complete native owner")
									})
								})
							}
						})
					}
				})
			}
		})
	}
}

func mustCheckpointDigest(t *testing.T, body []byte) string {
	t.Helper()
	var checkpoint db.ScopedCheckpoint
	require.NoError(t, json.Unmarshal(body, &checkpoint))
	return checkpoint.Cut.Digest
}

func TestScopedImportDistinctReceivingCustodyPrincipal(t *testing.T) {
	records, binding, local := scopedUpstreamRouteFixture(t, false)
	receivingActor := "receiving-member"
	binding.Actor = receivingActor
	var view *db.ScopedViewExport
	for _, record := range records {
		if row, ok := record.(*db.ScopedViewExport); ok {
			view = row
			row.AccountableActor = receivingActor
		}
	}
	require.NotNil(t, view)
	origin := db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: "00000000000000000000000005", Epoch: 3, AccountableActor: "origin-member", RouteDigest: view.RouteDigest}
	event := db.RemoteEvent{EventUID: "00000000000000000000000052", OriginInstanceUID: local, ProjectUID: binding.HubProjectUID, Type: "project.metadata_updated", Actor: "source-label", Payload: []byte(`{"revision_new":1,"diff":{"example":{"from":null,"to":true}}}`)}
	var err error
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	original, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	operation := db.SourceOperation{Version: 1, UID: "00000000000000000000000051", SourceInstanceUID: local, CommandKind: "patchProjectMetadata", Principal: origin, Members: []db.SourceMember{{ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: 0}}}
	operation.Digest, err = db.SourceOperationDigest(operation)
	require.NoError(t, err)
	manifest, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: operation, Originals: []db.RemoteEvent{event}, OriginalBytes: [][]byte{original}})
	require.NoError(t, err)
	originBytes, err := json.Marshal(origin)
	require.NoError(t, err)
	originKey, err := db.ScopedPrincipalKey(origin)
	require.NoError(t, err)
	receiver := origin
	receiver.BindingUID = binding.RelayConfig.BindingUID
	receiver.Epoch = binding.RelayConfig.ResetEpoch
	receiver.AccountableActor = receivingActor
	receiverBytes, err := json.Marshal(receiver)
	require.NoError(t, err)
	ordinal := int64(0)
	custody := &db.ScopedSourceCustodyExport{OperationUID: operation.UID, BindingUID: receiver.BindingUID, Epoch: receiver.Epoch, Direction: "inbound", PrincipalBytes: receiverBytes, ManifestDigest: operation.Digest, OwnsComplete: true, InvalidationState: "none"}
	baseRecords := append([]db.ImportRecord{}, records...)
	records = append(records, binding, &db.ScopedSourceOperationExport{OperationUID: operation.UID, Digest: operation.Digest, SourceInstanceUID: local, CommandKind: operation.CommandKind, ManifestBytes: manifest, PrincipalBytes: originBytes, PrincipalKey: originKey, State: "staged", OverlayState: "pending", RequestFingerprint: db.ScopedOfferedDigest([]byte("request"))}, &db.ScopedPrivateSourceExport{EventUID: event.EventUID, ProjectUID: event.ProjectUID, ContentHash: event.ContentHash, OperationUID: &operation.UID, Ordinal: &ordinal, EventBytes: original}, custody)
	t.Run("custody", func(t *testing.T) {
		require.NoError(t, db.ValidateImportRecords(records), "receiving authenticated account may differ from immutable origin principal")
		savedBinding, savedEpoch, savedActor := view.BindingUID, view.Epoch, view.AccountableActor
		view.BindingUID, view.Epoch, view.AccountableActor = origin.BindingUID, origin.Epoch, "different-hop-member"
		require.NoError(t, db.ValidateImportRecords(records), "native receiving hop proves its own account even when view indexes another hop")
		withoutNative := []db.ImportRecord{}
		for _, r := range records {
			if r != binding {
				withoutNative = append(withoutNative, r)
			}
		}
		require.Error(t, db.ValidateImportRecords(withoutNative), "without native context a different-hop view tuple cannot prove receiving actor")
		view.BindingUID, view.Epoch, view.AccountableActor = savedBinding, savedEpoch, savedActor
		require.NoError(t, db.ValidateImportRecords(withoutNative), "exact retained tuple preserves retired-owed custody without live permission")

		receiver.AccountableActor = "unrelated-member"
		custody.PrincipalBytes, err = json.Marshal(receiver)
		require.NoError(t, err)
		require.Error(t, db.ValidateImportRecords(records), "custody actor must still match its own authenticated retained hop")
	})
	t.Run("staged", func(t *testing.T) {
		header := db.ScopedDeliveryHeader{Version: 2, Domain: db.ScopedDeliveryDomain, BindingUID: binding.RelayConfig.BindingUID, Epoch: binding.RelayConfig.ResetEpoch, Channel: db.ScopedDeliverySource, Sequence: 1, AuthorityUID: view.AuthorityUID, SenderInstanceUID: binding.RelayConfig.UpstreamInstanceUID, ReceiverInstanceUID: local, Path: binding.RelayConfig.HubPath[:len(binding.RelayConfig.HubPath)-1], ObjectUID: operation.UID, ObjectDigest: operation.Digest}
		offer, err := db.EncodeScopedDelivery(header, manifest)
		require.NoError(t, err)
		exactHeader, err := db.ScopedDeliveryHeaderBytes(offer)
		require.NoError(t, err)
		stream := db.ScopedDeliveryStream{BindingUID: header.BindingUID, Epoch: header.Epoch, Direction: db.ScopedDeliveryInbound, Channel: header.Channel}
		staged := append(append([]db.ImportRecord{}, baseRecords...), binding, &db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: 1}, ObjectUID: operation.UID, ObjectDigest: operation.Digest, OfferedDigest: db.ScopedOfferedDigest(offer), HeaderBytes: exactHeader, EnvelopeBytes: offer}, &db.ScopedDeliveryCursor{ScopedDeliveryStream: stream, ReceivedSequence: 1, ReceivedDigest: db.ScopedOfferedDigest(offer)})
		require.NoError(t, db.ValidateImportRecords(staged), "staging does not equate immutable origin account with receiving native account")
		withoutNative := []db.ImportRecord{}
		for _, r := range staged {
			if r != binding {
				withoutNative = append(withoutNative, r)
			}
		}
		require.NoError(t, db.ValidateImportRecords(withoutNative), "exact retained tuple/route preserves pending inbound bytes after native teardown only")

		savedBinding, savedEpoch, savedActor := view.BindingUID, view.Epoch, view.AccountableActor
		view.BindingUID, view.Epoch, view.AccountableActor = origin.BindingUID, origin.Epoch, "different-hop-member"
		require.NoError(t, db.ValidateImportRecords(staged), "staging receiving native account is independent of a different indexed hop")
		require.Error(t, db.ValidateImportRecords(withoutNative), "missing native hop cannot borrow a different retained hop actor")

		view.BindingUID, view.Epoch, view.AccountableActor = savedBinding, savedEpoch, savedActor

	})

	var decoded db.SourceSubmission
	decoded, err = db.DecodeScopedSourceSubmission(manifest)
	require.NoError(t, err)
	require.Equal(t, origin, decoded.Operation.Principal)
}

func TestScopedImportCoverageBeforePublishedCut(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	coverage := f.Coverage
	coverage.Cut = db.ScopedCut{Sequence: 3, Digest: db.ScopedOfferedDigest([]byte("future-cut")), CaptureUID: "00000000000000000000000075"}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	raw, err := db.ScopedSourceCoverageSigningBytes(coverage)
	require.NoError(t, err)
	coverage.Signatures = []db.ScopedSignature{{ProjectUID: f.Operation.Members[0].ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	body, err := json.Marshal(coverage)
	require.NoError(t, err)
	digest, err := db.ScopedSourceCoverageObjectDigest(coverage)
	require.NoError(t, err)
	offered := fixtureInbound(t, f, db.ScopedDeliveryCoverage, coverage.OperationUID, digest, body)
	records := []db.ImportRecord{}
	var delivery *db.ScopedDeliveryRecord
	var cursor *db.ScopedDeliveryCursor
	for _, r := range offered {
		if _, ok := r.(*db.ScopedSourceCoverageExport); ok {
			continue
		}
		if op, ok := r.(*db.ScopedSourceOperationExport); ok {
			op.OverlayState = "pending"
		}
		if row, ok := r.(*db.ScopedDeliveryRecord); ok {
			delivery = row
			row.Accepted = false
		}
		if row, ok := r.(*db.ScopedDeliveryCursor); ok {
			cursor = row
			row.AcceptedSequence = 0
			row.AcceptedDigest = ""
		}
		records = append(records, r)
	}
	binding := &db.FederationBindingExport{ProjectID: 1, Role: "spoke", HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: f.Active.Projects[0].ProjectUID, Actor: f.Principal.AccountableActor, Enabled: true, RelayConfig: &db.RelayBindingConfig{ProtocolVersion: 2, BindingUID: f.Principal.BindingUID, ResetEpoch: 1, AuthorityUID: f.Principal.AuthorityUID, UpstreamInstanceUID: f.Principal.AuthorityUID, LocalActor: "member", HubPath: []string{f.Principal.AuthorityUID, f.View.DestinationUID}, ScopedView: &f.Active.Key, RootCutoverUID: f.View.RootCutoverUID}}
	records = append(records, binding)
	require.NoError(t, db.ValidateImportRecords(records), "unaccepted coverage before its cut remains exact pending admission, not verified coverage")
	delivery.Accepted = true
	cursor.AcceptedSequence = 1
	cursor.AcceptedDigest = delivery.OfferedDigest
	require.Error(t, db.ValidateImportRecords(records), "self-supplied cut is never authenticated expected coverage context")
	delivery.Accepted = false
	cursor.AcceptedSequence = 0
	cursor.AcceptedDigest = ""
	delivery.EnvelopeBytes = nil
	require.Error(t, db.ValidateImportRecords(records), "unverified pending coverage cannot compact")
}

func TestScopedImportCloseClockProofBinding(t *testing.T) {
	for _, variant := range []string{"legacy_unknown_null_value", "legacy_typed_without_boundary", "versioned_boundary", "missing_peer", "wrong_peer_project"} {
		t.Run(variant, func(t *testing.T) {
			f := dbtest.ScopedOwnerBackupFixture(t)
			records := []db.ImportRecord{}
			for _, r := range f.Records {
				if issue, ok := r.(*db.IssueExport); ok && issue.UID == "00000000000000000000000041" && variant == "missing_peer" {
					continue
				}
				if row, ok := r.(*db.ScopedCanonicalCloseExport); ok {
					if variant == "legacy_unknown_null_value" {
						row.ValueBytes = nil
					}
					if variant == "legacy_unknown_null_value" || variant == "legacy_typed_without_boundary" {
						proof, err := db.DecodeScopedCloseClockProof(row.StatusClockBytes)
						require.NoError(t, err)
						row.StatusClockBytes, err = json.Marshal(proof.StatusClock)
						require.NoError(t, err)
					}
					if variant == "versioned_boundary" {
						proof, err := db.DecodeScopedCloseClockProof(row.StatusClockBytes)
						require.NoError(t, err)
						clock := proof.StatusClock
						raw, err := db.EncodeScopedCloseClockProof(db.ScopedCloseClockProof{Version: 1, StatusClock: clock, CloseBoundaryClock: &clock})
						require.NoError(t, err)
						row.StatusClockBytes = append([]byte(" \n"), raw...)
					}
					if variant == "wrong_peer_project" {
						var value db.ScopedCloseValue
						require.NoError(t, json.Unmarshal(row.ValueBytes, &value))
						value.PeerProjectUID = f.Active.Projects[0].ProjectUID
						var err error
						row.ValueBytes, err = json.Marshal(value)
						require.NoError(t, err)
					}
				}
				records = append(records, r)
			}
			err := db.ValidateImportRecords(records)
			if variant == "legacy_unknown_null_value" || variant == "versioned_boundary" {
				require.NoError(t, err)
			} else if variant == "missing_peer" {
				require.NoError(t, err, "incoming close proof may arrive before its peer")
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestScopedImportRecordHistoricalTombstoneOptional(t *testing.T) {
	records, _, _ := scopedUpstreamRouteFixture(t, false)
	var generation *db.ScopedGenerationExport
	for _, r := range records {
		if row, ok := r.(*db.ScopedGenerationExport); ok {
			generation = row
		}
	}
	require.NotNil(t, generation)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	sign := func(raw []byte) []db.ScopedSignature {
		return []db.ScopedSignature{{ProjectUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	}
	var checkpoint db.ScopedCheckpoint
	require.NoError(t, json.Unmarshal(generation.CheckpointBytes, &checkpoint))
	checkpoint.Cut.Sequence = 3
	var err error
	checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
	require.NoError(t, err)
	transcript, err := db.ScopedCheckpointSigningBytes(checkpoint)
	require.NoError(t, err)
	checkpoint.Signatures = sign(transcript)
	generation.CheckpointBytes, err = json.Marshal(checkpoint)
	require.NoError(t, err)
	generation.CutSequence = 3
	generation.CutDigest = checkpoint.Cut.Digest
	generation.CheckpointDigest = &checkpoint.Cut.Digest
	effect := db.ScopedEffect{Collection: "issues", Key: "00000000000000000000000001:00000000000000000000000040", Action: "remove", Revision: 2}
	batch := db.ScopedBatch{View: checkpoint.View, Cut: db.ScopedCut{Sequence: 2, CaptureUID: "00000000000000000000000071"}, PreviousDigest: db.ScopedOfferedDigest([]byte("prior")), Effects: []db.ScopedEffect{effect}}
	batch.ComponentDigests, err = db.ScopedBatchComponentDigests(batch.Effects)
	require.NoError(t, err)
	batch.Cut.Digest, err = db.ScopedBatchDigest(batch)
	require.NoError(t, err)
	transcript, err = db.ScopedBatchSigningBytes(batch)
	require.NoError(t, err)
	batch.Signatures = sign(transcript)
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	records = append(records, &db.ScopedBatchExport{ViewUID: checkpoint.View.Key.ViewUID, Generation: 1, Sequence: 2, Digest: batch.Cut.Digest, PreviousDigest: batch.PreviousDigest, CaptureUID: batch.Cut.CaptureUID, BatchBytes: raw})
	require.NoError(t, db.ValidateImportRecords(records), "retained old remove proof does not require an obsolete tombstone")
	raw, err = json.Marshal(effect)
	require.NoError(t, err)
	subject := "00000000000000000000000040"
	row := &db.ScopedRecordExport{ViewUID: checkpoint.View.Key.ViewUID, Generation: 1, Collection: effect.Collection, RecordKey: effect.Key, ProjectUID: checkpoint.View.Projects[0].ProjectUID, SubjectUID: &subject, VisibleRevision: 2, Present: false, RecordBytes: raw, Digest: db.ScopedOfferedDigest(raw)}
	require.NoError(t, db.ValidateImportRecords(append(records, row)), "exact signed historical removal may remain while current checkpoint proves absence")
	effect.Revision = 4
	row.VisibleRevision = 4
	row.RecordBytes, err = json.Marshal(effect)
	require.NoError(t, err)
	row.Digest = db.ScopedOfferedDigest(row.RecordBytes)
	require.Error(t, db.ValidateImportRecords(append(records, row)), "future revision cannot borrow an older removal proof")
}

func TestScopedImportRecordCanonicalMetadataAssertion(t *testing.T) {
	records, _, _ := scopedUpstreamRouteFixture(t, false)
	var generation *db.ScopedGenerationExport
	for _, r := range records {
		if row, ok := r.(*db.ScopedGenerationExport); ok {
			generation = row
		}
	}
	var checkpoint db.ScopedCheckpoint
	require.NoError(t, json.Unmarshal(generation.CheckpointBytes, &checkpoint))
	project := checkpoint.View.Projects[0].ProjectUID
	checkpoint.Collections.Projects = []db.ScopedProjectState{{UID: project, Name: "visible", Metadata: db.JSONBlob(`{"z":9007199254740993,"a":2}`)}}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	var err error
	checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
	require.NoError(t, err)
	transcript, err := db.ScopedCheckpointSigningBytes(checkpoint)
	require.NoError(t, err)
	checkpoint.Signatures = []db.ScopedSignature{{ProjectUID: project, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, transcript))}}
	generation.CheckpointBytes, err = json.Marshal(checkpoint)
	require.NoError(t, err)
	generation.CheckpointDigest = &checkpoint.Cut.Digest
	generation.CutDigest = checkpoint.Cut.Digest
	generation.AssembledBytes = int64(len(generation.CheckpointBytes))
	require.NoError(t, db.ValidateScopedImportRecord(generation), "fixture must reach assertion comparison after valid generation shape")
	effect := db.ScopedEffect{Collection: "projects", Key: project, Action: "replace", Revision: checkpoint.Cut.Sequence, Project: &db.ScopedProjectState{UID: project, Name: "visible", Metadata: db.JSONBlob(" { \"a\":2, \"z\":9007199254740993 } ")}}
	raw, err := json.Marshal(effect)
	require.NoError(t, err)
	row := &db.ScopedRecordExport{ViewUID: checkpoint.View.Key.ViewUID, Generation: 1, Collection: "projects", RecordKey: project, ProjectUID: project, VisibleRevision: effect.Revision, Present: true, RecordBytes: raw, Digest: db.ScopedOfferedDigest(raw)}
	records = append(records, row)
	require.NoError(t, db.ValidateImportRecords(records), "derived assertion canonical-equivalent metadata preserves the same signed meaning")
	require.Equal(t, raw, []byte(row.RecordBytes), "corroboration cannot replace the exact retained row bytes")
	effect.Project.Metadata = db.JSONBlob(`{"a":2,"z":9007199254740992}`)
	row.RecordBytes, err = json.Marshal(effect)
	require.NoError(t, err)
	row.Digest = db.ScopedOfferedDigest(row.RecordBytes)
	require.Error(t, db.ValidateImportRecords(records), "an exact integer change remains different even beyond floating-point precision")
}

func TestScopedImportEnrollmentAccumulatedInboundPath(t *testing.T) {
	records, _, _ := scopedUpstreamRouteFixture(t, false)
	authority, relay, leaf := "00000000000000000000000009", "00000000000000000000000006", "00000000000000000000000008"
	var view *db.ScopedViewExport
	for _, r := range records {
		switch row := r.(type) {
		case *db.MetaKV:
			if row.Key == "instance_uid" {
				row.Value = authority
			}
		case *db.ScopedViewExport:
			view = row
		case *db.ScopedProjectExport:
			// This case moves the helper's local snapshot from leaf to root.
			// The root retains canonical ownership rather than an incoming view.
			row.OwnerViewUID, row.OwnerGeneration = nil, nil
		}
	}
	require.NotNil(t, view)
	records = append(records, &db.ScopedWorkExport{ViewUID: view.ViewUID, DirtyRevision: 1, State: "dirty"})
	projectID, parentToken := int64(1), int64(1)
	binding := "00000000000000000000000005"
	enrollment := &db.FederationEnrollmentExport{ID: 1, TokenHash: "retained-local-hash", SpokeInstanceUID: relay, ProjectID: &projectID, Actor: "relay-account", RelayBindingUID: &binding, RelayProtocolVersion: 2, ParentTokenID: &parentToken, RelayResetEpoch: 3, RelayServeDownstream: true}
	principal := db.CustodyPrincipal{AuthorityUID: authority, BindingUID: view.BindingUID, Epoch: view.Epoch, AccountableActor: "origin-account", RouteDigest: view.RouteDigest}
	event := db.RemoteEvent{EventUID: "00000000000000000000000052", OriginInstanceUID: leaf, ProjectUID: "00000000000000000000000001", Type: "project.metadata_updated", Actor: "source-label", Payload: []byte(`{"revision_new":1,"diff":{"example":{"from":null,"to":true}}}`)}
	var err error
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	native, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	op := db.SourceOperation{Version: 1, UID: "00000000000000000000000051", SourceInstanceUID: leaf, CommandKind: "patchProjectMetadata", Principal: principal, Members: []db.SourceMember{{ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: 0}}}
	op.Digest, err = db.SourceOperationDigest(op)
	require.NoError(t, err)
	body, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: op, Originals: []db.RemoteEvent{event}, OriginalBytes: [][]byte{native}})
	require.NoError(t, err)
	header := db.ScopedDeliveryHeader{Version: 2, Domain: db.ScopedDeliveryDomain, BindingUID: binding, Epoch: 3, Channel: db.ScopedDeliverySource, Sequence: 1, AuthorityUID: authority, SenderInstanceUID: relay, ReceiverInstanceUID: authority, Path: []string{leaf, relay}, ObjectUID: op.UID, ObjectDigest: op.Digest}
	staged := func(header db.ScopedDeliveryHeader) []db.ImportRecord {
		offer, err := db.EncodeScopedDelivery(header, body)
		require.NoError(t, err)
		raw, err := db.ScopedDeliveryHeaderBytes(offer)
		require.NoError(t, err)
		stream := db.ScopedDeliveryStream{BindingUID: binding, Epoch: 3, Direction: db.ScopedDeliveryInbound, Channel: db.ScopedDeliverySource}
		return append(append([]db.ImportRecord{}, records...), enrollment, &db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: 1}, ObjectUID: op.UID, ObjectDigest: op.Digest, OfferedDigest: db.ScopedOfferedDigest(offer), HeaderBytes: raw, EnvelopeBytes: offer}, &db.ScopedDeliveryCursor{ScopedDeliveryStream: stream, ReceivedSequence: 1, ReceivedDigest: db.ScopedOfferedDigest(offer)})
	}
	require.NoError(t, db.ValidateRelayDownstreamPath(authority, nil, header.Path), "native ingress permits accumulated downstream forwarding")
	require.NoError(t, db.ValidateImportRecords(staged(header)), "matching enrolled relay may forward exact pending leaf bytes without equating hop accounts")
	view.BindingUID, view.Epoch, view.AccountableActor = binding, 3, enrollment.Actor
	enrollment.RelayServeDownstream = false
	require.Error(t, db.ValidateImportRecords(staged(header)), "live leaf-only enrollment veto cannot be bypassed by retained exact tuple fallback")
	enrollment.RelayServeDownstream = true

	header.Path = []string{"00000000000000000000000010", relay}
	require.Error(t, db.ValidateImportRecords(staged(header)), "retained descriptor does not prove an unrelated path")
	header.Path = []string{leaf, relay, leaf, relay}
	_, err = db.EncodeScopedDelivery(header, body)
	require.Error(t, err, "cyclic path is rejected by the closed native header")
}

func TestScopedImportRetiredCustodyAfterNativeEpochAdvance(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	var old *db.ScopedViewExport
	for _, r := range f.Records {
		switch row := r.(type) {
		case *db.ScopedViewExport:
			old = row
			row.State = "retired"
			row.ActiveGeneration = nil
			row.PreparedGeneration = nil
		case *db.ScopedGenerationExport:
			row.State = "retired"
		}
	}
	require.NotNil(t, old)
	current := f.Active
	current.Key = db.ScopedViewKey{ViewUID: "00000000000000000000000021", Generation: 1}
	current.Route = append([]db.ScopedHopBinding{}, current.Route...)
	current.Route[0].Epoch = 2
	current.RouteDigest = db.ScopedOfferedDigest([]byte("current-epoch-route"))
	descriptor, err := json.Marshal(current)
	require.NoError(t, err)
	checkpoint := db.ScopedCheckpoint{View: current, Cut: db.ScopedCut{Sequence: 1, CaptureUID: "00000000000000000000000079"}}
	// Use the existing closed collection constructor's explicit empty shape.
	var retained db.ScopedCheckpoint
	for _, r := range f.Records {
		if row, ok := r.(*db.ScopedGenerationExport); ok && row.Generation == f.Active.Key.Generation {
			require.NoError(t, json.Unmarshal(row.CheckpointBytes, &retained))
		}
	}
	checkpoint.Collections = retained.Collections
	checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
	require.NoError(t, err)
	transcript, err := db.ScopedCheckpointSigningBytes(checkpoint)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	checkpoint.Signatures = []db.ScopedSignature{{ProjectUID: current.Projects[0].ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, transcript))}}
	body, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	generation := int64(1)
	view := &db.ScopedViewExport{ViewUID: current.Key.ViewUID, CatalogEntryUID: old.CatalogEntryUID, AuthorityUID: current.AuthorityUID, BindingUID: old.BindingUID, Epoch: 2, AccountableActor: "current-epoch-account", RouteDigest: current.RouteDigest, DestinationUID: current.DestinationUID, ActiveGeneration: &generation, State: "active", RequestDigest: current.RequestDigest}
	for _, r := range f.Records {
		if owner, ok := r.(*db.ScopedProjectExport); ok && owner.ProjectUID == current.Projects[0].ProjectUID {
			owner.OwnerViewUID = &view.ViewUID
			owner.OwnerGeneration = &generation
		}
	}
	binding := &db.FederationBindingExport{ProjectID: 1, Role: "spoke", HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: current.Projects[0].ProjectUID, Actor: view.AccountableActor, Enabled: true, RelayConfig: &db.RelayBindingConfig{ProtocolVersion: 2, BindingUID: view.BindingUID, ResetEpoch: 2, AuthorityUID: view.AuthorityUID, UpstreamInstanceUID: view.AuthorityUID, LocalActor: "member", HubPath: []string{view.AuthorityUID, view.DestinationUID}, ScopedView: &current.Key, RootCutoverUID: current.RootCutoverUID}}
	records := append(f.Records, view, &db.ScopedGenerationExport{ViewUID: view.ViewUID, Generation: 1, DescriptorBytes: descriptor, DescriptorDigest: db.ScopedOfferedDigest(descriptor), CheckpointBytes: body, CheckpointDigest: &checkpoint.Cut.Digest, CutSequence: 1, CutDigest: checkpoint.Cut.Digest, State: "active", AssembledBytes: int64(len(body)), RootCutoverUID: current.RootCutoverUID}, binding)
	require.NoError(t, db.ValidateImportRecords(records), "stable binding with newer epoch cannot erase independently authenticated retired old custody")
	old.AccountableActor = "unrelated-historical-account"
	require.Error(t, db.ValidateImportRecords(records), "new epoch cannot authenticate a different historical account")
}
