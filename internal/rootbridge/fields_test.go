package rootbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

func TestDecideFieldMerge(t *testing.T) {
	cases := []struct {
		name                     string
		baseline, kata, external connector.FieldValue
		want                     MergeAction
	}{
		{"unchanged", date("2026-08-20"), date("2026-08-20"), date("2026-08-20"), MergeAccept},
		{"kata only", date("2026-08-20"), date("2026-08-21"), date("2026-08-20"), MergeWriteExternal},
		{"external only", date("2026-08-20"), date("2026-08-20"), date("2026-08-22"), MergeWriteKata},
		{"same change", date("2026-08-20"), date("2026-08-22"), date("2026-08-22"), MergeAccept},
		{"divergent", date("2026-08-20"), date("2026-08-21"), date("2026-08-22"), MergeConflict},
		{"clear versus change", date("2026-08-20"), nullValue(), date("2026-08-22"), MergeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DecideFieldMerge(tc.baseline, tc.kata, tc.external).Action)
		})
	}
}

func TestDecideInitialFieldMerge(t *testing.T) {
	cases := []struct {
		name           string
		kata, external connector.FieldValue
		want           MergeAction
	}{
		{"both empty", connector.FieldValue{}, nullValue(), MergeAccept},
		{"same date", date("2026-08-20"), date("2026-08-20"), MergeAccept},
		{"kata only", date("2026-08-20"), nullValue(), MergeWriteExternal},
		{"external only", nullValue(), date("2026-08-20"), MergeWriteKata},
		{"different non-empty", date("2026-08-20"), date("2026-08-21"), MergeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DecideInitialFieldMerge(tc.kata, tc.external).Action)
		})
	}
}

func TestDecideFieldMergeKeepsBaselineAndCandidatesOnConflict(t *testing.T) {
	baseline := date("2026-08-20")
	kata := date("2026-08-21")
	external := date("2026-08-22")

	got := DecideFieldMerge(baseline, kata, external)

	assert.Equal(t, MergeConflict, got.Action)
	assert.Equal(t, baseline, got.Baseline)
	assert.Equal(t, kata, got.Kata)
	assert.Equal(t, external, got.External)
}

func TestLiveFieldDescriptorComparisonTreatsAcceptedKindsAsASet(t *testing.T) {
	mapping := db.ExternalFieldMapping{
		ExternalFieldID: "start-date", SchemaRevision: "schema-1",
		AcceptedKinds: []string{fieldKindDate, fieldKindInstant, fieldKindLocalDateTime},
		Nullable:      true, Writable: true,
	}
	descriptor := connector.FieldDescriptor{
		ID: "start-date", DisplayName: "Start date", SchemaRevision: "schema-1",
		AcceptedKinds: []string{fieldKindLocalDateTime, fieldKindDate, fieldKindInstant},
		Nullable:      true, Writable: true,
	}

	got, err := validateLiveFieldDescriptor(fieldCodecs["scheduled_on"], mapping, []connector.FieldDescriptor{descriptor})

	require.NoError(t, err)
	assert.Equal(t, descriptor, got)
}

func date(value string) connector.FieldValue {
	return connector.FieldValue{Kind: "date", Value: value}
}

func nullValue() connector.FieldValue {
	return connector.FieldValue{Kind: "null"}
}
