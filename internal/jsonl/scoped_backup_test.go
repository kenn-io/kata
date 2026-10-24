package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// This contract catches omission of a durable owed control result from an
// owner snapshot, as well as re-encoding its authenticated byte representation.
func TestScopedBackupExactBytes(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	principal := db.CustodyPrincipal{AuthorityUID: "00000000000000000000000009", BindingUID: "00000000000000000000000007", Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	principalRaw, err := json.Marshal(principal)
	require.NoError(t, err)
	request := db.ScopedControlRequest{RequestUID: "00000000000000000000000050", Digest: strings.Repeat("b", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: project.UID, IssueUID: "00000000000000000000000040"}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: source.InstanceUID(), Holder: "member", ClientKind: "cli"}}
	requestRaw, err := json.Marshal(request)
	require.NoError(t, err)
	requestRaw = append(append([]byte(" \n"), requestRaw...), []byte("\n ")...)
	result := []byte(" { \"request_uid\":\"00000000000000000000000050\", \"digest\":\"" + request.Digest + "\", \"state\":\"pending\", \"lease\":null, \"assignment\":null, \"reason_code\":\"\" } \n")
	_, err = source.ExecContext(ctx, `INSERT INTO federation_control_custody(request_uid,binding_uid,request_digest,principal_bytes,request_bytes,result_bytes,state,result_acknowledged,conversion_uid) VALUES(?,?,?,?,?,?,?,0,NULL)`, request.RequestUID, principal.BindingUID, request.Digest, principalRaw, requestRaw, result, "pending")
	require.NoError(t, err)
	history := seedScopedBackupHistory(t, source, project, principal, principalRaw)
	var backup bytes.Buffer
	require.NoError(t, Export(ctx, source, &backup, ExportOptions{}))
	require.Contains(t, backup.String(), `"kind":"federation_control_custody"`, "full owner export must include owed v2 controls")
	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
	var gotRequest, gotResult []byte
	require.NoError(t, target.QueryRowContext(ctx, `SELECT request_bytes,result_bytes FROM federation_control_custody WHERE request_uid=? AND binding_uid=?`, request.RequestUID, principal.BindingUID).Scan(&gotRequest, &gotResult))
	require.Equal(t, requestRaw, gotRequest)
	require.Equal(t, result, gotResult)
	for _, check := range history {
		var got []byte
		require.NoError(t, target.QueryRowContext(ctx, check.query, check.id).Scan(&got))
		require.Equal(t, check.body, got, check.query)
	}

	// Invalid graph or signature must fail before replacement becomes visible.
	_, err = target.CreateProject(ctx, "keep-target-project")
	require.NoError(t, err)
	var before bytes.Buffer
	require.NoError(t, Export(ctx, target, &before, ExportOptions{}))
	envelopes, err := NewDecoder(bytes.NewReader(backup.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	for _, failure := range []string{"missing_private_body", "bad_result_signature", "principal_index"} {
		t.Run(failure, func(t *testing.T) {
			var corrupt bytes.Buffer
			encoder := NewEncoder(&corrupt)
			for _, env := range envelopes {
				if failure == "missing_private_body" && env.Kind == Kind("federation_private_source") {
					continue
				}
				if env.Kind == Kind("federation_source_operation") && failure != "missing_private_body" {
					var row db.ScopedSourceOperationExport
					require.NoError(t, json.Unmarshal(env.Data, &row))
					if failure == "principal_index" {
						row.PrincipalKey = strings.Repeat("0", 64)
					} else {
						var result db.SourceResult
						require.NoError(t, json.Unmarshal(row.TerminalBytes, &result))
						result.ReasonCode = "changed-after-signing"
						row.TerminalBytes, err = json.Marshal(result)
						require.NoError(t, err)
					}
					env.Data, err = json.Marshal(row)
					require.NoError(t, err)
				}
				require.NoError(t, encoder.Write(env))
			}
			require.Error(t, Import(ctx, bytes.NewReader(corrupt.Bytes()), target))
			var after bytes.Buffer
			require.NoError(t, Export(ctx, target, &after, ExportOptions{}))
			require.Equal(t, before.Bytes(), after.Bytes(), "failed restore cannot clear even one target row")
		})
	}
	var public bytes.Buffer
	require.NoError(t, Export(ctx, source, &public, ExportOptions{ProjectID: project.ID}))
	for _, kind := range []string{"federation_control_custody", "federation_source_operation", "federation_private_source", "federation_source_custody"} {
		require.NotContains(t, public.String(), `"kind":"`+kind+`"`)
	}
}

type scopedBackupByteCheck struct {
	query, id string
	body      []byte
}

func seedScopedBackupHistory(t *testing.T, source *sqlitestore.Store, project db.Project, principal db.CustodyPrincipal, principalRaw []byte) []scopedBackupByteCheck {
	t.Helper()
	ctx := t.Context()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: principal.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, source.PinRootAuthority(ctx, pin))
	event := db.RemoteEvent{EventUID: "00000000000000000000000052", OriginInstanceUID: source.InstanceUID(), ProjectUID: project.UID, Type: "project.metadata_updated", Actor: "member", Payload: []byte("{ \n \"revision_new\" : 1, \"diff\" : { \"z\" : { \"from\" : null, \"to\" : 1 }, \"a\" : { \"from\" : null, \"to\" : 2 } } } ")}
	var err error
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	native, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	native = append(append([]byte(" \n"), native...), []byte("\n ")...)
	op := db.SourceOperation{Version: 1, UID: "00000000000000000000000051", SourceInstanceUID: source.InstanceUID(), CommandKind: "patchProjectMetadata", Principal: principal, Members: []db.SourceMember{{ProjectUID: project.UID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: 0}}}
	op.Digest, err = db.SourceOperationDigest(op)
	require.NoError(t, err)
	submission := db.SourceSubmission{Operation: op, Originals: []db.RemoteEvent{event}, OriginalBytes: [][]byte{native}}
	manifest, err := db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	manifest = append(append([]byte(" \n"), manifest...), []byte("\n ")...)
	admission, err := json.Marshal(db.ScopedAdmissionEvidence{Version: 1, OperationUID: op.UID, OperationDigest: op.Digest, Principal: principal})
	require.NoError(t, err)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: event.EventUID, ContentHash: event.ContentHash, AuthorityUID: principal.AuthorityUID, AccountableActor: principal.AccountableActor, SourceActor: event.Actor, IngressInstanceUID: source.InstanceUID(), AcceptedAt: time.Unix(1, 0).UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, key)
	require.NoError(t, err)
	receiptRaw, err := json.Marshal(receipt)
	require.NoError(t, err)
	receiptRaw = append(append([]byte(" \n"), receiptRaw...), []byte("\n ")...)
	result := db.SourceResult{OperationUID: op.UID, OperationDigest: op.Digest, State: "accepted", Members: op.Members, Receipts: []db.AttributionReceipt{receipt}}
	signing, err := db.ScopedSourceResultSigningBytes(result)
	require.NoError(t, err)
	result.Signatures = []db.ScopedSignature{{ProjectUID: project.UID, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, signing))}}
	terminal, err := json.Marshal(result)
	require.NoError(t, err)
	terminal = append(append([]byte(" \n"), terminal...), []byte("\n ")...)
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO federation_source_operations(operation_uid,digest,source_instance_uid,command_kind,manifest_bytes,principal_bytes,principal_key,state,overlay_state,admission_bytes,terminal_bytes,cancel_requested,idempotency_key,request_fingerprint) VALUES(?,?,?,?,?,?,?,'accepted','pending',?,?,0,NULL,?)`, op.UID, op.Digest, op.SourceInstanceUID, op.CommandKind, manifest, principalRaw, principalKey, admission, terminal, strings.Repeat("c", 64))
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO federation_private_sources(event_uid,content_hash,project_uid,operation_uid,ordinal,conversion_uid,event_bytes,provenance_bytes) VALUES(?,?,?,?,0,NULL,?,?)`, event.EventUID, event.ContentHash, event.ProjectUID, op.UID, native, receiptRaw)
	require.NoError(t, err)
	// This minimal history is local retained root admission, not a receiving-hop
	// custody claim. The complete twenty-family fixture supplies real hop context.
	// Signed bodies remain verifiable under independently retained old native pins.
	nextKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	nextPublic := nextKey.Public().(ed25519.PublicKey)
	next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
	transition, err := db.SignRootKeyTransition(pin, next, key)
	require.NoError(t, err)
	require.NoError(t, source.RotateRootAuthority(ctx, transition))
	return []scopedBackupByteCheck{{"SELECT manifest_bytes FROM federation_source_operations WHERE operation_uid=?", op.UID, manifest}, {"SELECT terminal_bytes FROM federation_source_operations WHERE operation_uid=?", op.UID, terminal}, {"SELECT event_bytes FROM federation_private_sources WHERE event_uid=?", event.EventUID, native}, {"SELECT provenance_bytes FROM federation_private_sources WHERE event_uid=?", event.EventUID, receiptRaw}}
}

func TestScopedImportRefusesCustodyTransforms(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	principal := db.CustodyPrincipal{AuthorityUID: "00000000000000000000000009", BindingUID: "00000000000000000000000007", Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	principalRaw, err := json.Marshal(principal)
	require.NoError(t, err)
	seedScopedBackupHistory(t, source, project, principal, principalRaw)
	request := db.ScopedControlRequest{RequestUID: "00000000000000000000000050", Digest: strings.Repeat("b", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: project.UID, IssueUID: "00000000000000000000000040"}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: source.InstanceUID(), Holder: "member", ClientKind: "cli"}}
	requestRaw, err := json.Marshal(request)
	require.NoError(t, err)
	result, err := json.Marshal(db.ScopedControlResult{RequestUID: request.RequestUID, Digest: request.Digest, State: "accepted"})
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO federation_control_custody(request_uid,binding_uid,request_digest,principal_bytes,request_bytes,result_bytes,state,result_acknowledged) VALUES(?,?,?,?,?,?,'accepted',0)`, request.RequestUID, principal.BindingUID, request.Digest, principalRaw, requestRaw, result)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, Export(ctx, source, &backup, ExportOptions{}))
	for _, tc := range []struct {
		name string
		opts ImportOptions
	}{{"standalone", ImportOptions{AsStandalone: true}}, {"new_instance", ImportOptions{NewInstance: true}}, {"project_merge", ImportOptions{MergeProject: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			identity := target.InstanceUID()
			var before bytes.Buffer
			require.NoError(t, Export(ctx, target, &before, ExportOptions{}))
			err = ImportWithOptions(ctx, bytes.NewReader(backup.Bytes()), target, tc.opts)
			require.ErrorContains(t, err, "custody", "unsupported transform must explicitly refuse owed custody on a genuinely fresh target")
			require.Equal(t, identity, target.InstanceUID())
			var after bytes.Buffer
			require.NoError(t, Export(ctx, target, &after, ExportOptions{}))
			require.Equal(t, before.Bytes(), after.Bytes(), "refusal must preserve fresh target identity/current state")
		})
	}
}
