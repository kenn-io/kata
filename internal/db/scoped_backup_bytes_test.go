package db

import (
	"bytes"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestScopedBackupNullableBytes(t *testing.T) {
	for _, tc := range []struct {
		bytes ScopedNullableBytes
		wire  string
	}{{nil, "null"}, {ScopedNullableBytes{}, `""`}, {ScopedNullableBytes{0, 255}, `"AP8="`}} {
		raw, err := json.Marshal(tc.bytes)
		require.NoError(t, err)
		require.Equal(t, tc.wire, string(raw))
		var got ScopedNullableBytes
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, tc.bytes, got)
	}
	var got ScopedNullableBytes
	require.Error(t, json.Unmarshal([]byte(`[]`), &got))
	require.Error(t, json.Unmarshal([]byte(`false`), &got))
}

func TestScopedBackupDeliveryByteCodec(t *testing.T) {
	header := ScopedDeliveryHeader{Version: 2, Domain: ScopedDeliveryDomain, BindingUID: verificationID(7), Epoch: 1, Channel: ScopedDeliveryControl, Sequence: 1, AuthorityUID: verificationID(9), SenderInstanceUID: verificationID(9), ReceiverInstanceUID: verificationID(8), Path: []string{verificationID(9)}, ObjectUID: verificationID(50), ObjectDigest: strings.Repeat("a", 64)}
	offered, err := EncodeScopedDelivery(header, []byte(" exact body \n"))
	require.NoError(t, err)
	exactHeader, err := ScopedDeliveryHeaderBytes(offered)
	require.NoError(t, err)
	for _, body := range [][]byte{nil, {}, offered} {
		row := ScopedDeliveryRecord{ScopedDeliveryRef: ScopedDeliveryRef{ScopedDeliveryStream: ScopedDeliveryStream{BindingUID: header.BindingUID, Epoch: 1, Direction: ScopedDeliveryInbound, Channel: header.Channel}, Sequence: 1}, ObjectUID: header.ObjectUID, ObjectDigest: header.ObjectDigest, OfferedDigest: ScopedOfferedDigest(offered), HeaderBytes: exactHeader, EnvelopeBytes: body, Accepted: true}
		raw, err := json.Marshal(row)
		require.NoError(t, err)
		if body == nil {
			require.Contains(t, string(raw), `"envelope_bytes":null`)
		} else if len(body) == 0 {
			require.Contains(t, string(raw), `"envelope_bytes":""`)
		}
		var got ScopedDeliveryRecord
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, row, got)
		unknown := append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"unknown":true}`)...)
		require.Error(t, json.Unmarshal(unknown, &got), "custom decoder must retain strict unknown-field rejection")
		bad := bytes.Replace(raw, []byte(`"accepted":true`), []byte(`"accepted":null`), 1)
		require.Error(t, json.Unmarshal(bad, &got))
	}
}
