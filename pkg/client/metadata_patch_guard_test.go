package client

import (
	"encoding/json"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

func TestMetadataPatchGuardValidationRequiresCondition(t *testing.T) {
	guard := generated.MetadataPatchGuard{}
	require.Error(t, guard.Validate())

	union := generated.MetadataPatchGuard_OneOf{}
	require.Error(t, union.Validate())
}

func TestMetadataPatchGuardValidationAcceptsEachCondition(t *testing.T) {
	valueUnion := generated.MetadataPatchGuard_OneOf{
		Either: runtime.NewEitherFromA[
			generated.MetadataPatchGuard_OneOf_0,
			generated.MetadataPatchGuard_OneOf_1,
		](generated.MetadataPatchGuard_OneOf_0{Key: "deck.rank", IfValue: `"current"`}),
	}
	require.NoError(t, valueUnion.Validate())
	require.NoError(t, (generated.MetadataPatchGuard{MetadataPatchGuard_OneOf: &valueUnion}).Validate())

	absentUnion := generated.MetadataPatchGuard_OneOf{
		Either: runtime.NewEitherFromB[
			generated.MetadataPatchGuard_OneOf_0,
			generated.MetadataPatchGuard_OneOf_1,
		](generated.MetadataPatchGuard_OneOf_1{
			Key:      "deck.rank",
			IfAbsent: generated.True,
		}),
	}
	require.NoError(t, absentUnion.Validate())
	require.NoError(t, (generated.MetadataPatchGuard{MetadataPatchGuard_OneOf: &absentUnion}).Validate())
}

func TestMetadataPatchGuardRequestSerializationRoundTrips(t *testing.T) {
	valueUnion := generated.MetadataPatchGuard_OneOf{
		Either: runtime.NewEitherFromA[
			generated.MetadataPatchGuard_OneOf_0,
			generated.MetadataPatchGuard_OneOf_1,
		](generated.MetadataPatchGuard_OneOf_0{Key: "deck.rank", IfValue: `"current"`}),
	}
	absentUnion := generated.MetadataPatchGuard_OneOf{
		Either: runtime.NewEitherFromB[
			generated.MetadataPatchGuard_OneOf_0,
			generated.MetadataPatchGuard_OneOf_1,
		](generated.MetadataPatchGuard_OneOf_1{
			Key:      "deck.rank",
			IfAbsent: generated.True,
		}),
	}

	tests := []struct {
		name  string
		union generated.MetadataPatchGuard_OneOf
		want  string
		isA   bool
	}{
		{name: "value", union: valueUnion, want: `{"patch":{"deck.rank":"next"},"guard":{"key":"deck.rank","if_value":"\"current\""}}`, isA: true},
		{name: "absent", union: absentUnion, want: `{"patch":{"deck.rank":"next"},"guard":{"key":"deck.rank","if_absent":true}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := generated.PatchIssueMetadataRequestBody{
				Guard: &generated.MetadataPatchGuard{MetadataPatchGuard_OneOf: &tt.union},
				Patch: map[string]any{"deck.rank": "next"},
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(raw))

			var roundTrip generated.PatchIssueMetadataRequestBody
			require.NoError(t, json.Unmarshal(raw, &roundTrip))
			require.NotNil(t, roundTrip.Guard)
			require.NoError(t, roundTrip.Guard.Validate())
			require.Equal(t, tt.isA, roundTrip.Guard.MetadataPatchGuard_OneOf.IsA())
		})
	}
}
