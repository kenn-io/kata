package db

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func scopedImportSourceFixture(t *testing.T) ([]ImportRecord, *ScopedSourceOperationExport, *ScopedPrivateSourceExport) {
	t.Helper()
	v, pins, _ := verificationView(t)
	event := RemoteEvent{EventUID: verificationID(51), OriginInstanceUID: verificationID(8), ProjectUID: pins[0].ProjectUID, Type: "project.metadata_updated", Actor: "member", Payload: []byte("{ \n \"revision_new\" : 1, \"diff\" : { \"z\" : { \"from\" : null, \"to\" : 1 }, \"a\" : { \"from\" : null, \"to\" : 2 } } } ")}
	var err error
	event.ContentHash, err = RemoteEventContentHash(event)
	require.NoError(t, err)
	native, err := EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	principal := CustodyPrincipal{AuthorityUID: v.AuthorityUID, BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: v.RouteDigest}
	op := SourceOperation{Version: 1, UID: verificationID(50), SourceInstanceUID: event.OriginInstanceUID, CommandKind: "patchProjectMetadata", Principal: principal, Members: []SourceMember{{ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: 0}}}
	op.Digest, err = SourceOperationDigest(op)
	require.NoError(t, err)
	submission := SourceSubmission{Operation: op, Originals: []RemoteEvent{event}, OriginalBytes: [][]byte{native}}
	wire, err := EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	wire = append(append([]byte(" \n"), wire...), []byte("\n ")...)
	principalRaw, err := json.Marshal(principal)
	require.NoError(t, err)
	principalKey, err := ScopedPrincipalKey(principal)
	require.NoError(t, err)
	operation := &ScopedSourceOperationExport{OperationUID: op.UID, Digest: op.Digest, SourceInstanceUID: op.SourceInstanceUID, CommandKind: op.CommandKind, ManifestBytes: wire, PrincipalBytes: principalRaw, PrincipalKey: principalKey, State: "staged", OverlayState: "pending", RequestFingerprint: strings.Repeat("a", 64)}
	ordinal := int64(0)
	original := &ScopedPrivateSourceExport{EventUID: event.EventUID, ProjectUID: event.ProjectUID, ContentHash: event.ContentHash, OperationUID: &operation.OperationUID, Ordinal: &ordinal, EventBytes: native}
	records := []ImportRecord{&MetaKV{Key: "instance_uid", Value: event.OriginInstanceUID}, &ProjectExport{ID: 1, UID: event.ProjectUID, Name: "example-project"}, &pins[0], operation, original}
	return records, operation, original
}

func TestScopedImportRejectsIncompleteCustody(t *testing.T) {
	records, op, original := scopedImportSourceFixture(t)
	require.NoError(t, ValidateImportRecords(records), "complete local retained submission/native original graph must be restorable")
	require.Error(t, ValidateImportRecords(append(append([]ImportRecord{}, records[:4]...), records[5:]...)), "local complete source retention requires every original body")
	bad := *original
	bad.EventBytes = bytes.ReplaceAll(original.EventBytes, []byte("member"), []byte("another-member"))
	changed := append([]ImportRecord{}, records...)
	changed[4] = &bad
	require.Error(t, ValidateImportRecords(changed), "retained original hash/index mismatch")
	badOp := *op
	badOp.PrincipalKey = strings.Repeat("0", 64)
	changed = append([]ImportRecord{}, records...)
	changed[3] = &badOp
	require.Error(t, ValidateImportRecords(changed), "principal key comes from authenticated tuple")
	badOp = *op
	badOp.Digest = strings.Repeat("0", 64)
	changed[3] = &badOp
	require.Error(t, ValidateImportRecords(changed), "manifest index cannot disagree with retained whole submission")
	decoded, err := DecodeScopedSourceSubmission(op.ManifestBytes)
	require.NoError(t, err)
	require.Equal(t, original.EventBytes, decoded.OriginalBytes[0])
	require.Equal(t, op.ManifestBytes, decoded.WireBytes)
	t.Run("canonical_original_agreement", func(t *testing.T) {
		event, err := DecodeRelaySourceEvent(original.EventBytes)
		require.NoError(t, err)
		canonical := &EventExport{ID: 1, UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectID: 1, ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, ContentHash: event.ContentHash, CreatedAt: event.CreatedAt.Format(time.RFC3339Nano)}
		matching := append(append([]ImportRecord{}, records...), canonical)
		require.NoError(t, ValidateImportRecords(matching), "matching canonical/private originals are a complete valid owner graph")
		changed := event
		changed.Actor = "another-member"
		changed.ContentHash, err = RemoteEventContentHash(changed)
		require.NoError(t, err)
		conflict := *canonical
		conflict.Actor, conflict.ContentHash = changed.Actor, changed.ContentHash
		require.NoError(t, validateImportRecord(&conflict), "the conflicting canonical copy has a valid row shape and recomputed native hash")
		contradictory := append(append([]ImportRecord{}, records...), &conflict)
		require.Error(t, ValidateImportRecords(contradictory), "one native event UID cannot retain two different authenticated originals")
	})
}
