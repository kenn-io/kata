package jsonl

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestScopedImportRejectsScalarNull(t *testing.T) {
	id := "00000000000000000000000001"
	for _, tc := range []struct {
		name     string
		row      db.ImportRecord
		from, to string
	}{
		{"serving_fenced", &db.ScopedProjectExport{ProjectUID: id, ProtocolFloor: 1}, `"serving_fenced":false`, `"serving_fenced":null`},
		{"dirty_revision", &db.ScopedWorkExport{ViewUID: id, State: "idle"}, `"dirty_revision":0`, `"dirty_revision":null`},
		{"reason_code", &db.ScopedViewExport{ViewUID: id, CatalogEntryUID: id, AuthorityUID: id, BindingUID: id, Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64), DestinationUID: "00000000000000000000000002", State: "retired", RequestDigest: strings.Repeat("b", 64)}, `"reason_code":""`, `"reason_code":null`},
		{"acknowledged_digest", &db.ScopedDeliveryCursor{ScopedDeliveryStream: db.ScopedDeliveryStream{BindingUID: id, Epoch: 1, Direction: db.ScopedDeliveryInbound, Channel: db.ScopedDeliverySource}}, `"acknowledged_digest":""`, `"acknowledged_digest":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.row)
			require.NoError(t, err)
			_, ok, err := decodeScopedImportRecord(Envelope{Kind: Kind(tc.row.ImportKind()), Data: raw})
			require.True(t, ok)
			require.NoError(t, err, "valid zero-valued scalar and nullable pointers must pass")
			changed := bytes.Replace(raw, []byte(tc.from), []byte(tc.to), 1)
			require.NotEqual(t, raw, changed)
			_, ok, err = decodeScopedImportRecord(Envelope{Kind: Kind(tc.row.ImportKind()), Data: changed})
			require.True(t, ok)
			require.Error(t, err, "nonnull persisted scalar must reject explicit JSON null before zero coercion")
		})
	}
}
func TestScopedImportAllowsNullableBodies(t *testing.T) {
	id := "00000000000000000000000001"
	principal := db.CustodyPrincipal{AuthorityUID: id, BindingUID: id, Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	principalRaw, err := json.Marshal(principal)
	require.NoError(t, err)
	request := db.ScopedControlRequest{RequestUID: id, Digest: strings.Repeat("b", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: id, IssueUID: "00000000000000000000000002"}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: id, Holder: "member", ClientKind: "cli"}}
	requestRaw, err := json.Marshal(request)
	require.NoError(t, err)
	row := &db.ScopedControlCustodyExport{RequestUID: id, BindingUID: id, RequestDigest: request.Digest, PrincipalBytes: principalRaw, RequestBytes: requestRaw, State: "pending"}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"result_bytes":null`)
	got, ok, err := decodeScopedImportRecord(Envelope{Kind: Kind(row.ImportKind()), Data: raw})
	require.True(t, ok)
	require.NoError(t, err)
	require.Nil(t, got.(*db.ScopedControlCustodyExport).ResultBytes)
}

func TestScopedImportAllowsNullCheckpoint(t *testing.T) {
	id := "00000000000000000000000001"
	cutover := "00000000000000000000000060"
	descriptor := db.ScopedViewDescriptor{Version: 2, Schema: 1, Key: db.ScopedViewKey{ViewUID: id, Generation: 1}, AuthorityUID: id, DestinationUID: "00000000000000000000000002", RouteDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), RootCutoverUID: cutover, Projects: []db.ScopedProjectPin{{ProjectUID: id, Pin: db.RootKeyPin{ProjectUID: id, AuthorityUID: id, KeyID: db.RootPublicKeyID(bytes.Repeat([]byte{1}, 32)), PublicKey: bytes.Repeat([]byte{1}, 32)}}}, Route: []db.ScopedHopBinding{{BindingUID: id, Epoch: 1, InstanceUID: "00000000000000000000000002"}}}
	rawDescriptor, err := json.Marshal(descriptor)
	require.NoError(t, err)
	row := db.ScopedGenerationExport{ViewUID: id, Generation: 1, DescriptorBytes: rawDescriptor, DescriptorDigest: db.ScopedOfferedDigest(rawDescriptor), State: "prepared", RootCutoverUID: cutover}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"checkpoint_bytes":null`)
	got, ok, err := decodeScopedImportRecord(Envelope{Kind: Kind(row.ImportKind()), Data: raw})
	require.True(t, ok)
	require.NoError(t, err)
	require.Nil(t, got.(*db.ScopedGenerationExport).CheckpointBytes)
}
