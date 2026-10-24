package jsonl

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestScopedImportActiveCapturePreparedExpansion(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	require.NoError(t, db.ValidateImportRecords(f.Records), "in-flight old active capture remains owed while larger prepared generation exists")
	// A valid capture for only the peer matches neither p nor p+q descriptor.
	capture := db.CanonicalViewState{CaptureUID: *f.Work.CaptureUID, ProjectCuts: map[string]int64{f.View.Projects[1].ProjectUID: 1}, Projects: []db.Project{{UID: f.View.Projects[1].ProjectUID, Name: "peer-project"}}}
	raw, err := db.EncodeScopedCanonicalCapture(capture)
	require.NoError(t, err)
	f.Work.CaptureBytes = raw
	digest := db.ScopedOfferedDigest(raw)
	f.Work.CaptureDigest = &digest

	require.Error(t, db.ValidateImportRecords(f.Records))
}

func TestScopedImportInventoryCommitments(t *testing.T) {
	for _, kind := range []string{"coverage", "capture", "conversion"} {
		t.Run(kind, func(t *testing.T) {
			f := dbtest.ScopedOwnerBackupFixture(t)
			fixturePreparedCapture(t, &f)
			base := *f.Obligations[0]
			base.Kind = kind
			switch kind {
			case "coverage":
				base.ObjectUID = f.Operation.UID
				var err error
				base.Digest, err = db.ScopedSourceCoverageObjectDigest(f.Coverage)
				require.NoError(t, err)
			case "capture":
				base.ObjectUID = *f.Work.CaptureUID
				base.Digest = *f.Work.CaptureDigest
			case "conversion":
				var request db.ScopedConversionRequest
				require.NoError(t, json.Unmarshal(f.Conversion.RequestBytes, &request))
				var err error
				base.ObjectUID = request.UID
				base.Digest, err = db.ScopedConversionRequestObjectDigest(request)
				require.NoError(t, err)
			}
			records := append(append([]db.ImportRecord{}, f.Records[:len(f.Records)-1]...), &base)
			require.NoError(t, db.ValidateImportRecords(records), "exact retained object inventory must restore")
			bad := base
			bad.ObjectUID = "00000000000000000000000099"
			records[len(records)-1] = &bad
			require.Error(t, db.ValidateImportRecords(records), "digest never substitutes for object identity")
			bad = base
			switch kind {
			case "coverage":
				bad.Digest = f.Coverage.Cut.Digest
			case "capture":
				for _, r := range f.Records {
					if g, ok := r.(*db.ScopedGenerationExport); ok {
						bad.Digest = g.DescriptorDigest
						break
					}
				}
			case "conversion":
				bad.Digest = f.Conversion.RequestDigest
			}
			records[len(records)-1] = &bad
			require.Error(t, db.ValidateImportRecords(records), "component/request digest is not full object inventory commitment")
		})
	}
}

func TestScopedImportCompactedCoverageAndOrder(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	fixturePreparedCapture(t, &f)
	for _, record := range f.Records {
		if delivery, ok := record.(*db.ScopedDeliveryRecord); ok {
			delivery.EnvelopeBytes = nil
		}
	}
	require.NoError(t, db.ValidateImportRecords(f.Records), "accepted hop may compact when exact independent certificate remains")
	slices.Reverse(f.Records)
	require.NoError(t, db.ValidateImportRecords(f.Records), "complete dependency graph is input-order independent")
}

func TestScopedBackupAllFamilies(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	var exactClock []byte
	for _, record := range f.Records {
		if row, ok := record.(*db.ScopedCanonicalCloseExport); ok {
			exactClock = append(append([]byte(" \n"), row.StatusClockBytes...), []byte("\n ")...)
			row.StatusClockBytes = exactClock
		}
	}
	require.NotEmpty(t, exactClock)
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	require.NoError(t, source.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
	var storedClock []byte
	require.NoError(t, source.QueryRowContext(t.Context(), `SELECT status_clock_bytes FROM federation_canonical_close_state`).Scan(&storedClock))
	require.Equal(t, exactClock, storedClock)
	var first bytes.Buffer
	require.NoError(t, Export(t.Context(), source, &first, ExportOptions{}))
	envelopes, err := NewDecoder(bytes.NewReader(first.Bytes())).ReadAll(t.Context())
	require.NoError(t, err)
	previous := -1
	for _, envelope := range envelopes {
		if rank := db.ScopedImportRank(string(envelope.Kind)); rank >= 0 {
			require.GreaterOrEqual(t, rank, previous, "owner kinds follow frozen dependency order")
			previous = rank
		}
	}
	target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, Import(t.Context(), bytes.NewReader(first.Bytes()), target))
	require.NoError(t, target.QueryRowContext(t.Context(), `SELECT status_clock_bytes FROM federation_canonical_close_state`).Scan(&storedClock))
	require.Equal(t, exactClock, storedClock, "JSONL round trip preserves supplied versioned clock bytes")
	var after bytes.Buffer
	require.NoError(t, Export(t.Context(), target, &after, ExportOptions{}))
	restored, err := NewDecoder(bytes.NewReader(after.Bytes())).ReadAll(t.Context())
	require.NoError(t, err)
	expectedScoped, actualScoped := []Envelope{}, []Envelope{}
	for _, e := range envelopes {
		if db.ScopedImportRank(string(e.Kind)) >= 0 {
			expectedScoped = append(expectedScoped, e)
		}
	}
	for _, e := range restored {
		if db.ScopedImportRank(string(e.Kind)) >= 0 {
			actualScoped = append(actualScoped, e)
		}
	}
	require.Equal(t, len(expectedScoped), len(actualScoped))
	families := map[Kind]bool{}
	for n, e := range expectedScoped {
		families[e.Kind] = true
		require.Equal(t, e.Kind, actualScoped[n].Kind)
		require.True(t, bytes.Equal(e.Data, actualScoped[n].Data), "exact owner row changed: %s row %d", e.Kind, n)
	}
	require.Len(t, families, 20, "every persisted family survives")
	var active, prepared int64
	require.NoError(t, target.QueryRowContext(t.Context(), `SELECT active_generation,prepared_generation FROM federation_scoped_views`).Scan(&active, &prepared))
	require.Equal(t, int64(2), active)
	require.Equal(t, int64(3), prepared)
	require.NoError(t, Import(t.Context(), bytes.NewReader(first.Bytes()), target), "second restore atomically replaces prior custody")
}

// Isolate inventory tests from the separate old-active/new-prepared capture case.
func fixturePreparedCapture(t *testing.T, f *dbtest.ScopedOwnerFixture) {
	t.Helper()
	capture := db.CanonicalViewState{CaptureUID: *f.Work.CaptureUID, ProjectCuts: map[string]int64{}}
	for _, project := range f.View.Projects {
		capture.ProjectCuts[project.ProjectUID] = 1
		capture.Projects = append(capture.Projects, db.Project{UID: project.ProjectUID, Name: "example-project"})
	}
	raw, err := db.EncodeScopedCanonicalCapture(capture)
	require.NoError(t, err)
	f.Work.CaptureBytes = raw
	digest := db.ScopedOfferedDigest(raw)
	f.Work.CaptureDigest = &digest
}

func TestScopedImportControlPrincipalOrder(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	fixturePreparedCapture(t, &f)
	principal := f.Principal
	principal.Epoch = 2
	raw, err := json.Marshal(principal)
	require.NoError(t, err)
	f.Control.PrincipalBytes = raw
	key, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	f.Obligations[0].PrincipalKey = key
	f.Obligations[0].Epoch = principal.Epoch
	require.NoError(t, db.ValidateImportRecords(f.Records))
	slices.Reverse(f.Records)
	require.NoError(t, db.ValidateImportRecords(f.Records), "control-only principal index must be ready before reference validation")
}

func fixtureInbound(t *testing.T, f dbtest.ScopedOwnerFixture, channel db.ScopedDeliveryChannel, objectUID, digest string, body []byte) []db.ImportRecord {
	t.Helper()
	header := db.ScopedDeliveryHeader{Version: 2, Domain: db.ScopedDeliveryDomain, BindingUID: f.Principal.BindingUID, Epoch: f.Principal.Epoch, Channel: channel, Sequence: 1, AuthorityUID: f.Principal.AuthorityUID, SenderInstanceUID: f.Principal.AuthorityUID, ReceiverInstanceUID: f.View.DestinationUID, Path: []string{f.Principal.AuthorityUID}, ObjectUID: objectUID, ObjectDigest: digest}
	offer, err := db.EncodeScopedDelivery(header, body)
	require.NoError(t, err)
	exactHeader, err := db.ScopedDeliveryHeaderBytes(offer)
	require.NoError(t, err)
	stream := db.ScopedDeliveryStream{BindingUID: header.BindingUID, Epoch: header.Epoch, Direction: db.ScopedDeliveryInbound, Channel: channel}
	records := []db.ImportRecord{}
	for _, r := range f.Records {
		switch r.(type) {
		case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
		default:
			records = append(records, r)
		}
	}
	return append(records, &db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: 1}, ObjectUID: objectUID, ObjectDigest: digest, OfferedDigest: db.ScopedOfferedDigest(offer), HeaderBytes: exactHeader, EnvelopeBytes: offer, Accepted: true}, &db.ScopedDeliveryCursor{ScopedDeliveryStream: stream, ReceivedSequence: 1, ReceivedDigest: db.ScopedOfferedDigest(offer), AcceptedSequence: 1, AcceptedDigest: db.ScopedOfferedDigest(offer)})
}

func TestScopedImportDeliveryRetainedAuthority(t *testing.T) {
	for _, variant := range []string{"control_subject", "control_action", "control_result_request_digest", "source_outer_whitespace", "conversion_request"} {
		t.Run(variant, func(t *testing.T) {
			f := dbtest.ScopedOwnerBackupFixture(t)
			fixturePreparedCapture(t, &f)
			var records []db.ImportRecord
			switch variant {
			case "control_subject", "control_action":
				var request db.ScopedControlRequest
				require.NoError(t, json.Unmarshal(f.Control.RequestBytes, &request))
				if variant == "control_subject" {
					request.Subject.IssueUID = "00000000000000000000000099"
				} else {
					request.Action.Force = true
				}
				digest, err := db.ScopedControlRequestObjectDigest(request)
				require.NoError(t, err)
				raw, err := json.Marshal(request)
				require.NoError(t, err)
				records = fixtureInbound(t, f, db.ScopedDeliveryControl, request.RequestUID, digest, raw)
			case "control_result_request_digest":
				result := db.ScopedControlResult{RequestUID: f.Control.RequestUID, Digest: db.ScopedOfferedDigest([]byte("another-request")), State: "pending"}
				digest, err := db.ScopedControlResultObjectDigest(result)
				require.NoError(t, err)
				raw, err := json.Marshal(result)
				require.NoError(t, err)
				records = fixtureInbound(t, f, db.ScopedDeliveryControl, result.RequestUID, digest, raw)
			case "source_outer_whitespace":
				for _, r := range f.Records {
					if op, ok := r.(*db.ScopedSourceOperationExport); ok {
						records = fixtureInbound(t, f, db.ScopedDeliverySource, op.OperationUID, op.Digest, bytes.TrimSpace(op.ManifestBytes))
					}
				}
			case "conversion_request":
				var request db.ScopedConversionRequest
				require.NoError(t, json.Unmarshal(f.Conversion.RequestBytes, &request))
				request.SuspendBindingUIDs = []string{"00000000000000000000000099"}
				digest, err := db.ScopedConversionRequestObjectDigest(request)
				require.NoError(t, err)
				raw, err := json.Marshal(request)
				require.NoError(t, err)
				records = fixtureInbound(t, f, db.ScopedDeliveryConversion, request.UID, digest, raw)
			}
			require.Error(t, db.ValidateImportRecords(records), "fresh valid hop header cannot replace retained immutable request/submission authority")
		})
	}
}

func TestScopedImportHistoricalControlAndConversionOffers(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	fixturePreparedCapture(t, &f)
	pending := db.ScopedControlResult{RequestUID: f.Control.RequestUID, Digest: f.Control.RequestDigest, State: "pending"}
	pendingRaw, err := json.Marshal(pending)
	require.NoError(t, err)
	terminal := pending
	terminal.State = "accepted"
	f.Control.ResultBytes, err = json.Marshal(terminal)
	require.NoError(t, err)
	f.Control.State = "accepted"
	digest, err := db.ScopedControlResultObjectDigest(pending)
	require.NoError(t, err)
	require.NoError(t, db.ValidateImportRecords(fixtureInbound(t, f, db.ScopedDeliveryControl, pending.RequestUID, digest, pendingRaw)), "historical pending result shares immutable request, not current terminal phase")
	var request db.ScopedConversionRequest
	require.NoError(t, json.Unmarshal(f.Conversion.RequestBytes, &request))
	old := db.ScopedConversionState{Request: request, Phase: "prepare", OldBindings: []db.ScopedOldBinding{}}
	cutover := f.View.RootCutoverUID
	inventory := db.ScopedOfferedDigest([]byte("inventory"))
	f.Conversion.RootCutoverUID = &cutover
	f.Conversion.InventoryDigest = &inventory
	f.Conversion.Phase = "fence"
	current := old
	current.Phase = "fence"
	current.RootCutoverUID = cutover
	current.InventoryDigest = inventory
	f.Conversion.ResultBytes, err = json.Marshal(current)
	require.NoError(t, err)
	oldRaw, err := json.Marshal(old)
	require.NoError(t, err)
	digest, err = db.ScopedConversionStateObjectDigest(old)
	require.NoError(t, err)
	require.NoError(t, db.ValidateImportRecords(fixtureInbound(t, f, db.ScopedDeliveryConversion, request.UID, digest, oldRaw)), "historical conversion phase is retained under its immutable request")
	t.Run("contradictory_terminal", func(t *testing.T) {
		f := dbtest.ScopedOwnerBackupFixture(t)
		fixturePreparedCapture(t, &f)
		retained := db.ScopedControlResult{RequestUID: f.Control.RequestUID, Digest: f.Control.RequestDigest, State: "rejected"}
		f.Control.State = retained.State
		f.Control.ResultBytes, err = json.Marshal(retained)
		require.NoError(t, err)
		digest, err := db.ScopedControlResultObjectDigest(retained)
		require.NoError(t, err)
		valid := fixtureInbound(t, f, db.ScopedDeliveryControl, retained.RequestUID, digest, f.Control.ResultBytes)
		require.NoError(t, db.ValidateImportRecords(valid), "a consistent terminal offer is a complete valid owner graph")
		contradictory := retained
		contradictory.State = "accepted"
		raw, err := json.Marshal(contradictory)
		require.NoError(t, err)
		digest, err = db.ScopedControlResultObjectDigest(contradictory)
		require.NoError(t, err)
		changed := fixtureInbound(t, f, db.ScopedDeliveryControl, contradictory.RequestUID, digest, raw)
		t.Run("validation", func(t *testing.T) {
			require.Error(t, db.ValidateImportRecords(changed), "the same immutable control request cannot have conflicting terminal outcomes")
		})
		for _, backend := range []string{"sqlite", "postgres"} {
			t.Run(backend, func(t *testing.T) {
				target := artifactBackupStore(t, backend)
				require.NoError(t, target.ImportReplay(t.Context(), valid, db.ImportOptions{}), "consistent terminal graph restores before the negative")
				var before bytes.Buffer
				require.NoError(t, Export(t.Context(), target, &before, ExportOptions{IncludeDeleted: true}))
				require.Error(t, target.ImportReplay(t.Context(), changed, db.ImportOptions{}))
				var after bytes.Buffer
				require.NoError(t, Export(t.Context(), target, &after, ExportOptions{IncludeDeleted: true}))
				require.Equal(t, before.Bytes(), after.Bytes(), "contradictory terminal replacement is atomic")
			})
		}
	})
}

func TestScopedImportRollbackAfterCustodyInsert(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	fixturePreparedCapture(t, &f)
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	require.NoError(t, source.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
	var backup bytes.Buffer
	require.NoError(t, Export(t.Context(), source, &backup, ExportOptions{}))
	// An injected target-local I/O boundary aborts after replacement has begun.
	// It is not production DDL and does not change any import validation rule.
	_, err = source.ExecContext(t.Context(), `CREATE TRIGGER scoped_test_interrupt BEFORE INSERT ON federation_control_custody BEGIN SELECT RAISE(ABORT,'injected custody interruption'); END`)
	require.NoError(t, err)
	err = Import(t.Context(), bytes.NewReader(backup.Bytes()), source)
	require.ErrorContains(t, err, "injected custody interruption")
	var after bytes.Buffer
	require.NoError(t, Export(t.Context(), source, &after, ExportOptions{}))
	require.Equal(t, backup.Bytes(), after.Bytes(), "failed in-transaction replay preserves all preexisting owner custody")
}

func TestScopedImportUnacceptedInboundStaging(t *testing.T) {
	for _, channel := range []db.ScopedDeliveryChannel{db.ScopedDeliverySource, db.ScopedDeliveryControl} {
		t.Run(string(channel), func(t *testing.T) {
			f := dbtest.ScopedOwnerBackupFixture(t)
			fixturePreparedCapture(t, &f)
			var body []byte
			var objectUID, digest string
			for _, r := range f.Records {
				if op, ok := r.(*db.ScopedSourceOperationExport); ok && channel == db.ScopedDeliverySource {
					body = op.ManifestBytes
					objectUID = op.OperationUID
					digest = op.Digest
				}
			}
			if channel == db.ScopedDeliveryControl {
				body = f.Control.RequestBytes
				objectUID = f.Control.RequestUID
				var request db.ScopedControlRequest
				require.NoError(t, json.Unmarshal(body, &request))
				var err error
				digest, err = db.ScopedControlRequestObjectDigest(request)
				require.NoError(t, err)
			}
			offered := fixtureInbound(t, f, channel, objectUID, digest, body)
			records := []db.ImportRecord{}
			var delivery *db.ScopedDeliveryRecord
			var cursor *db.ScopedDeliveryCursor
			for _, r := range offered {
				switch r.(type) {
				case *db.ScopedConversionObligationExport:
					continue
				case *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedSourceDependencyExport, *db.ScopedSourceCoverageExport:
					if channel == db.ScopedDeliverySource {
						continue
					}
				case *db.ScopedControlCustodyExport:
					if channel == db.ScopedDeliveryControl {
						continue
					}
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
			require.NoError(t, db.ValidateImportRecords(records), "complete unaccepted authenticated-hop staging requires no invented admission row")
			delivery.Accepted = true
			cursor.AcceptedSequence = 1
			cursor.AcceptedDigest = delivery.OfferedDigest
			require.Error(t, db.ValidateImportRecords(records), "accepted custody must have independent complete logical state")
			delivery.Accepted = false
			cursor.AcceptedSequence = 0
			cursor.AcceptedDigest = ""
			delivery.Direction = db.ScopedDeliveryOutbound
			cursor.Direction = db.ScopedDeliveryOutbound
			cursor.ReceivedSequence = 0
			cursor.ReceivedDigest = ""
			require.Error(t, db.ValidateImportRecords(records), "outbound always requires independently retained custody")
			delivery.Direction = db.ScopedDeliveryInbound
			cursor.Direction = db.ScopedDeliveryInbound
			cursor.ReceivedSequence = 1
			cursor.ReceivedDigest = delivery.OfferedDigest
			exact := delivery.EnvelopeBytes
			delivery.EnvelopeBytes = nil
			require.Error(t, db.ValidateImportRecords(records), "unaccepted staging cannot compact")
			delivery.EnvelopeBytes = exact
			retired := records[:len(records)-1]
			require.NoError(t, db.ValidateImportRecords(retired), "exact retained authenticated tuple remains custody-only recovery context")
			header, body, err := db.DecodeScopedDelivery(exact)
			require.NoError(t, err)
			header.BindingUID = "00000000000000000000000099"
			header.Epoch = 42
			changed, err := db.EncodeScopedDelivery(header, body)
			require.NoError(t, err)
			delivery.EnvelopeBytes = changed
			delivery.HeaderBytes, err = db.ScopedDeliveryHeaderBytes(changed)
			require.NoError(t, err)
			delivery.BindingUID = header.BindingUID
			delivery.Epoch = header.Epoch
			delivery.OfferedDigest = db.ScopedOfferedDigest(changed)
			cursor.ScopedDeliveryStream = delivery.ScopedDeliveryStream
			cursor.ReceivedDigest = delivery.OfferedDigest
			require.Error(t, db.ValidateImportRecords(retired), "unaccepted offer cannot invent a missing retained authenticated tuple/route")
		})
	}
}

func TestScopedImportRootIdleFixture(t *testing.T) {
	f := dbtest.ScopedOwnerRootIdleFixture(t)
	require.NoError(t, db.ValidateImportRecords(f.Records))
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "root.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
	require.Equal(t, f.Principal.AuthorityUID, store.InstanceUID())
	var capture []byte
	var dirty int64
	require.NoError(t, store.QueryRowContext(t.Context(), `SELECT capture_bytes,dirty_revision FROM federation_scoped_work`).Scan(&capture, &dirty))
	require.Nil(t, capture)
	require.Equal(t, int64(8), dirty)
}

func TestScopedImportPrefixesRequireEveryRetainedIdentity(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	fixturePreparedCapture(t, &f)
	var request db.ScopedControlRequest
	require.NoError(t, json.Unmarshal(f.Control.RequestBytes, &request))
	digest, err := db.ScopedControlRequestObjectDigest(request)
	require.NoError(t, err)
	records := fixtureInbound(t, f, db.ScopedDeliveryControl, request.RequestUID, digest, f.Control.RequestBytes)
	first := records[len(records)-2].(*db.ScopedDeliveryRecord)
	cursor := records[len(records)-1].(*db.ScopedDeliveryCursor)
	header, body, err := db.DecodeScopedDelivery(first.EnvelopeBytes)
	require.NoError(t, err)
	header.Sequence = 2
	secondOffer, err := db.EncodeScopedDelivery(header, body)
	require.NoError(t, err)
	secondHeader, err := db.ScopedDeliveryHeaderBytes(secondOffer)
	require.NoError(t, err)
	second := *first
	second.Sequence = 2
	second.HeaderBytes = secondHeader
	second.EnvelopeBytes = secondOffer
	second.OfferedDigest = db.ScopedOfferedDigest(secondOffer)
	cursor.ReceivedSequence = 2
	cursor.ReceivedDigest = second.OfferedDigest
	cursor.AcceptedSequence = 2
	cursor.AcceptedDigest = second.OfferedDigest
	first.EnvelopeBytes = nil // Keep the immutable old retry identity after compaction.
	records = append(records, &second)
	require.NoError(t, db.ValidateImportRecords(records))
	withoutOld := []db.ImportRecord{}
	for _, record := range records {
		if record != first {
			withoutOld = append(withoutOld, record)
		}
	}
	require.Error(t, db.ValidateImportRecords(withoutOld), "endpoint digest cannot prove old per-sequence identity")
	require.Greater(t, db.ScopedRetainedBytes(first), int64(len(first.HeaderBytes)), "tombstone header and identity still count toward finite quotas")
}
