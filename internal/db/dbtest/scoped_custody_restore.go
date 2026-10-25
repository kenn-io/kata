package dbtest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Start with the accepted complete owner graph, then change only the retained
// source outcome/token correspondence. No native admission or authority is
// faked by these restore fixtures.
func scopedCustodyExpectationGraph(t *testing.T, root bool, state string, slot int) []db.ImportRecord {
	t.Helper()
	f := ScopedOwnerBackupFixture(t)
	if root {
		f = ScopedOwnerRootIdleFixture(t)
	}
	require.NoError(t, db.ValidateImportRecords(f.Records), "baseline must reach token correspondence")
	var records []db.ImportRecord
	var operation *db.ScopedSourceOperationExport
	var original *db.ScopedPrivateSourceExport
	for _, record := range f.Records {
		switch row := record.(type) {
		case *db.ScopedSourceCoverageExport, *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
			// Coverage is a separate signed object, unrelated to these custody
			// outcomes. Retain the complete manifest/original/dependency graph.
			continue
		case *db.ScopedSourceOperationExport:
			operation = row
		case *db.ScopedPrivateSourceExport:
			original = row
		}
		records = append(records, record)
	}
	require.NotNil(t, operation)
	require.NotNil(t, original)
	submission, err := db.DecodeScopedSourceSubmission(operation.ManifestBytes)
	require.NoError(t, err)
	if slot != 0 {
		submission.Operation.UID = scopedOwnerUID(951)
		submission.Originals[0].EventUID = scopedOwnerUID(952)
		submission.Originals[0].ContentHash, err = db.RemoteEventContentHash(submission.Originals[0])
		require.NoError(t, err)
		submission.Operation.Members[0].EventUID = submission.Originals[0].EventUID
		submission.Operation.Members[0].ContentHash = submission.Originals[0].ContentHash
		submission.OriginalBytes[0], err = db.EncodeRelaySourceEvent(submission.Originals[0])
		require.NoError(t, err)
		original.EventUID = submission.Originals[0].EventUID
		original.ContentHash = submission.Originals[0].ContentHash
		original.EventBytes = submission.OriginalBytes[0]
	}
	submission.WireBytes = nil
	submission.Operation.Expectations = []db.MutationExpectation{{Token: "opaque-preparation-token"}}
	submission.Operation.Digest, err = db.SourceOperationDigest(submission.Operation)
	require.NoError(t, err)
	op := submission.Operation
	operation.OperationUID, operation.Digest = op.UID, op.Digest
	operation.ManifestBytes, err = db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	operation.RequestFingerprint = db.ScopedOfferedDigest(operation.ManifestBytes)
	operation.AdmissionBytes = scopedOwnerBytes(t, db.ScopedAdmissionEvidence{Version: 1, OperationUID: op.UID, OperationDigest: op.Digest, Principal: op.Principal})
	operation.State = state
	operation.OverlayState = "pending"
	var result db.SourceResult
	require.NoError(t, db.DecodeScopedRecord(operation.TerminalBytes, &result))
	operation.TerminalBytes = nil
	if state != "staged" {
		result.OperationUID, result.OperationDigest = op.UID, op.Digest
		result.State, result.Members = state, op.Members
		result.Signatures = nil
		if state != "accepted" {
			result.Receipts = nil
			result.ReasonCode = "guard_conflict"
			if state == "canceled" {
				result.ReasonCode = "source_canceled"
			}
		} else if slot != 0 {
			receipt := result.Receipts[0]
			receipt.EventUID, receipt.ContentHash = op.Members[0].EventUID, op.Members[0].ContentHash
			receipt.Sequence++
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
			result.Receipts[0], err = db.SignRootReceipt(receipt, key)
			require.NoError(t, err)
		}
		raw, err := db.ScopedSourceResultSigningBytes(result)
		require.NoError(t, err)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
		result.Signatures = []db.ScopedSignature{{ProjectUID: op.Members[0].ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
		operation.TerminalBytes = scopedOwnerBytes(t, result)
		if state != "accepted" {
			operation.OverlayState = "settled"
		}
	}
	original.OperationUID = &operation.OperationUID
	if state != "accepted" {
		original.ProvenanceBytes = nil
	} else {
		original.ProvenanceBytes = scopedOwnerBytes(t, result.Receipts[0])
	}
	for _, record := range records {
		switch row := record.(type) {
		case *db.ScopedSourceCustodyExport:
			row.OperationUID, row.ManifestDigest = op.UID, op.Digest
		case *db.ScopedSourceDependencyExport:
			row.OperationUID = op.UID
		case *db.ScopedMutationExpectationExport:
			if state == "accepted" {
				row.ConsumedOperationUID = &operation.OperationUID
			}
		}
	}
	return records
}

func scopedWithoutExpectationRows(records []db.ImportRecord) []db.ImportRecord {
	var result []db.ImportRecord
	for _, record := range records {
		switch record.(type) {
		case *db.ScopedMutationExpectationExport, *db.ScopedPredicateIncarnationExport:
			continue
		}
		result = append(result, record)
	}
	return result
}

// Retain an independently valid new binding/epoch tuple P2. The old issuer
// token row remains self-bound to P1; referencing it confers no authority on P2.
func scopedCustodyNewPrincipal(t *testing.T, records []db.ImportRecord) []db.ImportRecord {
	t.Helper()
	view := ScopedOwnerBackupFixture(t).View
	view.Key = db.ScopedViewKey{ViewUID: scopedOwnerUID(985), Generation: 1}
	view.Route = []db.ScopedHopBinding{{BindingUID: scopedOwnerUID(986), InstanceUID: view.DestinationUID, Epoch: 2}}
	view.RouteDigest = strings.Repeat("b", 64)
	principal := db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: 2, AccountableActor: "renewed-member", RouteDigest: view.RouteDigest}
	principalRaw := scopedOwnerBytes(t, principal)
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	descriptor := scopedOwnerBytes(t, view)
	records = append(records,
		&db.ScopedViewExport{ViewUID: view.Key.ViewUID, CatalogEntryUID: scopedOwnerUID(987), AuthorityUID: view.AuthorityUID, BindingUID: principal.BindingUID, Epoch: principal.Epoch, AccountableActor: principal.AccountableActor, RouteDigest: principal.RouteDigest, DestinationUID: view.DestinationUID, PreparedGeneration: scopedOwnerPtr(int64(1)), State: "prepared", RequestDigest: view.RequestDigest},
		&db.ScopedGenerationExport{ViewUID: view.Key.ViewUID, Generation: 1, DescriptorBytes: descriptor, DescriptorDigest: db.ScopedOfferedDigest(descriptor), State: "prepared", RootCutoverUID: view.RootCutoverUID},
		&db.ScopedWorkExport{ViewUID: view.Key.ViewUID, DirtyRevision: 1, State: "idle"},
	)
	var operation *db.ScopedSourceOperationExport
	for _, record := range records {
		if row, ok := record.(*db.ScopedSourceOperationExport); ok {
			operation = row
		}
	}
	require.NotNil(t, operation)
	submission, err := db.DecodeScopedSourceSubmission(operation.ManifestBytes)
	require.NoError(t, err)
	submission.WireBytes = nil
	submission.Operation.Principal = principal
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	for n := range submission.Operation.Membership {
		proof := &submission.Operation.Membership[n]
		raw, err := db.ScopedMembershipSigningBytes(*proof, principal)
		require.NoError(t, err)
		proof.Signatures = []db.ScopedSignature{{ProjectUID: proof.Endpoint.ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	}
	submission.Operation.Digest, err = db.SourceOperationDigest(submission.Operation)
	require.NoError(t, err)
	op := submission.Operation
	operation.Digest, operation.PrincipalBytes, operation.PrincipalKey = op.Digest, principalRaw, principalKey
	operation.ManifestBytes, err = db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	operation.RequestFingerprint = db.ScopedOfferedDigest(operation.ManifestBytes)
	operation.AdmissionBytes = scopedOwnerBytes(t, db.ScopedAdmissionEvidence{Version: 1, OperationUID: op.UID, OperationDigest: op.Digest, Principal: principal})
	var provenance []byte
	if operation.TerminalBytes != nil {
		var result db.SourceResult
		require.NoError(t, db.DecodeScopedRecord(operation.TerminalBytes, &result))
		result.OperationDigest = op.Digest
		if result.State == "accepted" {
			receipt := result.Receipts[0]
			receipt.AccountableActor = principal.AccountableActor
			result.Receipts[0], err = db.SignRootReceipt(receipt, key)
			require.NoError(t, err)
			provenance = scopedOwnerBytes(t, result.Receipts[0])
		}
		raw, err := db.ScopedSourceResultSigningBytes(result)
		require.NoError(t, err)
		result.Signatures = []db.ScopedSignature{{ProjectUID: op.Members[0].ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
		operation.TerminalBytes = scopedOwnerBytes(t, result)
	}
	for _, record := range records {
		switch row := record.(type) {
		case *db.ScopedSourceCustodyExport:
			row.BindingUID, row.Epoch, row.PrincipalBytes, row.ManifestDigest = principal.BindingUID, principal.Epoch, principalRaw, op.Digest
		case *db.ScopedSourceDependencyExport:
			row.ProofBytes = scopedOwnerBytes(t, op.Membership[0])
		case *db.ScopedPrivateSourceExport:
			row.ProvenanceBytes = provenance
		}
	}
	return records
}

// Opaque forwarding is durable owner custody, including a retained signed root
// disposition. A root accepted manifest alone requires exact consumed evidence.
func RunScopedCustodyOpaqueTokenOwnerRestore(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, absent := range []string{"token", "consumption"} {
		t.Run("accepted_missing_instance_uid_with_missing_"+absent, func(t *testing.T) {
			records := scopedCustodyExpectationGraph(t, true, "accepted", 0)
			require.NoError(t, db.ValidateImportRecords(records))
			var fragment []db.ImportRecord
			for _, record := range records {
				if meta, ok := record.(*db.MetaKV); ok && meta.Key == "instance_uid" {
					continue
				}
				fragment = append(fragment, record)
			}
			// Owner identity is mandatory and rejects before token validation.
			// The mutations below do not provide token-specific coverage.
			require.ErrorContains(t, db.ValidateImportRecords(fragment), "scoped owner snapshot requires its instance UID")
			if absent == "token" {
				fragment = scopedWithoutExpectationRows(fragment)
			} else {
				for _, record := range fragment {
					if token, ok := record.(*db.ScopedMutationExpectationExport); ok {
						token.ConsumedOperationUID = nil
					}
				}
			}
			require.Error(t, db.ValidateImportRecords(fragment), "missing metadata is not affirmative proof of a nonroot owner")
			target := open(t)
			before := scopedAuthoritySnapshot(t, target)
			require.Error(t, target.ImportReplay(t.Context(), fragment, db.ImportOptions{}))
			require.Equal(t, before, scopedAuthoritySnapshot(t, target), "invalid fragment cannot preserve target identity and bypass root evidence")
		})
	}
	for _, root := range []bool{false, true} {
		for _, state := range []string{"staged", "accepted", "rejected", "canceled"} {
			name := "hop_" + state
			if root {
				name = "root_" + state
			}
			t.Run(name, func(t *testing.T) {
				records := scopedCustodyExpectationGraph(t, root, state, 0)
				require.NoError(t, db.ValidateImportRecords(records), "complete local token counterpart control")
				records = scopedWithoutExpectationRows(records)
				if root && state == "accepted" {
					require.ErrorContains(t, db.ValidateImportRecords(records), "expectation token missing")
					return
				}
				require.NoError(t, db.ValidateImportRecords(records), "opaque retention does not import root predicate rows")
				owner := open(t)
				require.NoError(t, owner.ImportReplay(t.Context(), records, db.ImportOptions{}))
				exported := scopedAuthoritySnapshot(t, owner)
				require.NoError(t, db.ValidateImportRecords(exported))
				restored := open(t)
				require.NoError(t, restored.ImportReplay(t.Context(), exported, db.ImportOptions{}))
				require.Equal(t, exported, scopedAuthoritySnapshot(t, restored))
			})
		}
	}
	for _, state := range []string{"rejected", "canceled", "accepted"} {
		t.Run("root_reused_consumed_token_"+state, func(t *testing.T) {
			records := scopedCustodyExpectationGraph(t, true, "accepted", 0)
			require.NoError(t, db.ValidateImportRecords(records), "op A accepted with exact token consumption")
			second := scopedCustodyExpectationGraph(t, true, state, 1)
			for _, record := range second {
				switch record.(type) {
				case *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedSourceDependencyExport:
					records = append(records, record)
				}
			}
			if state == "accepted" {
				require.Error(t, db.ValidateImportRecords(records), "op B cannot be accepted using op A's consumed token")
				var unknownIdentity []db.ImportRecord
				for _, record := range records {
					if meta, ok := record.(*db.MetaKV); ok && meta.Key == "instance_uid" {
						continue
					}
					unknownIdentity = append(unknownIdentity, record)
				}
				require.Error(t, db.ValidateImportRecords(unknownIdentity), "omitting identity cannot hide accepted B's wrong consumption")
				return
			}
			require.NoError(t, db.ValidateImportRecords(records), "same-principal rejected/canceled reuse preserves private outcome")
			owner := open(t)
			require.NoError(t, owner.ImportReplay(t.Context(), records, db.ImportOptions{}))
			exported := scopedAuthoritySnapshot(t, owner)
			require.NoError(t, db.ValidateImportRecords(exported))
			require.NoError(t, open(t).ImportReplay(t.Context(), exported, db.ImportOptions{}))
		})
	}
	for _, state := range []string{"staged", "rejected", "canceled", "accepted"} {
		t.Run("root_old_token_new_principal_"+state, func(t *testing.T) {
			second := scopedCustodyExpectationGraph(t, true, state, 1)
			require.NoError(t, db.ValidateImportRecords(second), "unchanged principal counterpart")
			second = scopedCustodyNewPrincipal(t, second)
			if state == "accepted" {
				// Token consumption names B exactly, isolating principal mismatch.
				require.ErrorContains(t, db.ValidateImportRecords(second), "token belongs to another principal")
				return
			}
			records := scopedCustodyExpectationGraph(t, true, "accepted", 0)
			for _, record := range second {
				switch row := record.(type) {
				case *db.ScopedSourceOperationExport, *db.ScopedPrivateSourceExport, *db.ScopedSourceCustodyExport, *db.ScopedSourceDependencyExport:
					records = append(records, record)
				case *db.ScopedViewExport:
					if row.ViewUID == scopedOwnerUID(985) {
						records = append(records, row)
					}
				case *db.ScopedGenerationExport:
					if row.ViewUID == scopedOwnerUID(985) {
						records = append(records, row)
					}
				case *db.ScopedWorkExport:
					if row.ViewUID == scopedOwnerUID(985) {
						records = append(records, row)
					}
				}
			}
			require.NoError(t, db.ValidateImportRecords(records), "old token reference grants no authority; negative custody remains restorable")
			owner := open(t)
			require.NoError(t, owner.ImportReplay(t.Context(), records, db.ImportOptions{}))
			exported := scopedAuthoritySnapshot(t, owner)
			require.NoError(t, db.ValidateImportRecords(exported))
			require.NoError(t, open(t).ImportReplay(t.Context(), exported, db.ImportOptions{}))
		})
	}
}

// The issuing root and the receiving intermediate have separate native stores.
// Both the intermediate's incoming binding and outgoing parent-linked grant
// are real. The opaque token is never imported with root-private issuer rows.
func RunScopedCustodyOpaqueIntermediateRestore(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	root := open(t)
	origin := NewScopedAuthorityFixture(t, root)
	owner := open(t)
	upstream, projects := NewScopedIncomingAuthorityFixture(t, owner)
	InstallScopedIncomingAuthority(t, owner, upstream, projects, true)
	parent, err := owner.ResolveAPIToken(t.Context(), "incoming-local-token")
	require.NoError(t, err)
	child := upstream
	child.Key = db.ScopedViewKey{ViewUID: scopedOwnerUID(980), Generation: 1}
	child.Route = append(append([]db.ScopedHopBinding{}, upstream.Route...), db.ScopedHopBinding{BindingUID: scopedOwnerUID(981), InstanceUID: scopedOwnerUID(982), Epoch: 1})
	child.DestinationUID = child.Route[len(child.Route)-1].InstanceUID
	params := db.CreateScopedRelayEnrollmentParams{View: child, CatalogEntryUID: scopedOwnerUID(983), AnchorProjectID: projects[0].ID, ParentTokenID: parent.ID, Token: "intermediate-downstream-token", ServeDownstream: false}
	grant, err := owner.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), params)
	require.NoError(t, err)
	f := ScopedCaptureFixture{ScopedAuthorityFixture: ScopedAuthorityFixture{Params: params, Parent: parent, Projects: projects, Issue: origin.Issue}, Grant: grant}
	_, receiving, submission := newScopedCustodySubmission(t, owner, f)
	// This fixture helper defaults to the first hop; the actual receiving
	// native grant here is the second hop issued by this intermediate.
	receiving.BindingUID = child.Route[1].BindingUID
	receiving.Epoch = child.Route[1].Epoch
	submission.Operation.Principal = receiving
	proof := submission.Operation.Membership[0]
	raw, err := db.ScopedMembershipSigningBytes(proof, receiving)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	proof.Signatures = []db.ScopedSignature{{ProjectUID: origin.Issue.ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	submission.Operation.Membership = []db.ScopedMembershipProof{proof}
	submission.Operation.Expectations = []db.MutationExpectation{{Token: "opaque-root-token-not-installed-at-intermediate"}}
	submission.WireBytes = nil
	submission.Operation.Digest, err = db.SourceOperationDigest(submission.Operation)
	require.NoError(t, err)
	wire, err := db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	submission, err = db.DecodeScopedSourceSubmission(append(append([]byte(" \n"), wire...), '\n'))
	require.NoError(t, err)
	ctx := db.WithScopedSourceAccess(t.Context(), db.ScopedSourceAccess{View: child.Key, Principal: receiving})
	status, err := scopedCustodyNative(t, owner).RetainSourceOperation(ctx, receiving, submission)
	require.NoError(t, err, "real intermediate retains the opaque complete source")
	require.Equal(t, "staged", status.State)
	exported := scopedAuthoritySnapshot(t, owner)
	var incoming, outgoing bool
	for _, record := range exported {
		switch row := record.(type) {
		case *db.ScopedMutationExpectationExport, *db.ScopedPredicateIncarnationExport:
			t.Fatal("intermediate must not invent root predicate state")
		case *db.ScopedSourceOperationExport:
			require.Equal(t, submission.WireBytes, row.ManifestBytes)
		case *db.ScopedSourceCustodyExport:
			if row.OperationUID != submission.Operation.UID {
				continue
			}
			principal, evidence, err := db.DecodeScopedSourceCustodyPrincipal(row.PrincipalBytes)
			require.NoError(t, err)
			require.NotNil(t, evidence)
			require.Equal(t, child.Key, evidence.View)
			require.Equal(t, db.ScopedOfferedDigest(scopedOwnerBytes(t, child)), evidence.DescriptorDigest)
			if row.Direction == "inbound" {
				require.Equal(t, receiving, principal)
				incoming = true
			} else {
				require.Equal(t, upstream.Route[0].BindingUID, principal.BindingUID)
				require.Equal(t, "member", principal.AccountableActor)
				require.NotEqual(t, receiving.AccountableActor, principal.AccountableActor)
				outgoing = true
			}
		}
	}
	require.True(t, incoming)
	require.True(t, outgoing)
	require.NoError(t, db.ValidateImportRecords(exported), "complete intermediate owner backup preserves opaque custody")
	restored := open(t)
	require.NoError(t, restored.ImportReplay(t.Context(), exported, db.ImportOptions{}))
	require.Equal(t, exported, scopedAuthoritySnapshot(t, restored))
}
