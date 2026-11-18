package rootbridge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

func TestScheduleCodecsAreRegistered(t *testing.T) {
	for _, key := range []string{"scheduled_on", "deadline_on"} {
		codec, ok := fieldCodecs[key]
		require.True(t, ok)
		assert.Equal(t, key, codec.KataField())
	}
}

func TestScheduleCodecReadKataCanonicalizesValues(t *testing.T) {
	codec := fieldCodecs["scheduled_on"]
	cases := []struct {
		name string
		meta string
		want connector.FieldValue
	}{
		{"empty", `{}`, nullValue()},
		{"date", `{"scheduled_on":"2026-08-20"}`, date("2026-08-20")},
		{"local minute", `{"scheduled_on":"2026-08-20T09:30","timezone":"America/New_York"}`, connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T09:30", Timezone: "America/New_York"}},
		{"local second", `{"scheduled_on":"2026-08-20T09:30:45","timezone":"America/New_York"}`, connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T09:30:45", Timezone: "America/New_York"}},
		{"utc instant", `{"scheduled_on":"2026-08-20T09:30:45.120000000Z"}`, connector.FieldValue{Kind: "instant", Value: "2026-08-20T09:30:45.12Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codec.ReadKata(db.Issue{Metadata: db.JSONBlob(tc.meta)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestScheduleCodecReadKataRejectsInvalidTimezone(t *testing.T) {
	_, err := fieldCodecs["scheduled_on"].ReadKata(db.Issue{Metadata: db.JSONBlob(`{"scheduled_on":"2026-08-20T09:30","timezone":"Invalid/Zone"}`)})
	require.Error(t, err)
}

func TestScheduleCodecReadKataUsesConfiguredAndUTCFallbacks(t *testing.T) {
	issue := db.Issue{Metadata: db.JSONBlob(`{"scheduled_on":"2026-08-20T09:30"}`)}

	configured, err := (scheduleCodec{
		key: "scheduled_on", defaultTimezone: "America/Chicago",
	}).ReadKata(issue)
	require.NoError(t, err)
	assert.Equal(t, connector.FieldValue{
		Kind: "local_datetime", Value: "2026-08-20T09:30", Timezone: "America/Chicago",
	}, configured)

	utc, err := fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, err)
	assert.Equal(t, "UTC", utc.Timezone)
}

func TestBridgeConstructorsCarryConfiguredTimezoneToCodecs(t *testing.T) {
	issue := db.Issue{Metadata: db.JSONBlob(`{"scheduled_on":"2026-08-20T09:30"}`)}
	reconciler := NewReconciler(nil, nil, ReconcilerConfig{DefaultTimezone: "Europe/Paris"})
	service := NewServiceWithEventSinkAndDefaultTimezone(nil, nil, nil, nil, "Europe/Paris")

	reconciled, err := reconciler.fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, err)
	resolved, err := service.fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", reconciled.Timezone)
	assert.Equal(t, "Europe/Paris", resolved.Timezone)
}

func TestScheduleCodecKataPatchPreservesCivilTimesAcrossDST(t *testing.T) {
	codec := fieldCodecs["scheduled_on"]
	for _, value := range []connector.FieldValue{
		{Kind: "local_datetime", Value: "2026-03-08T02:30", Timezone: "America/New_York"},
		{Kind: "local_datetime", Value: "2026-11-01T01:30:45", Timezone: "America/New_York"},
	} {
		patch, err := codec.KataPatch(db.Issue{Metadata: db.JSONBlob(`{"deadline_on":"2026-08-20T09:30[Europe/Paris]","timezone":"America/New_York"}`)}, value)
		require.NoError(t, err)
		assert.JSONEq(t, `"`+value.Value+`[America/New_York]"`, string(patch["scheduled_on"]))
		assert.NotContains(t, patch, "timezone")
	}
}

func TestScheduleCodecKataPatchCanonicalizesAndClears(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value connector.FieldValue
		want  string
	}{
		{"scheduled date", "scheduled_on", date("2026-08-24"), `{"scheduled_on":"2026-08-24[UTC]"}`},
		{"scheduled instant", "scheduled_on", connector.FieldValue{Kind: "instant", Value: "2026-08-24T07:30:00.12Z"}, `{"scheduled_on":"2026-08-24T07:30:00.12Z"}`},
		{"deadline local", "deadline_on", connector.FieldValue{Kind: "local_datetime", Value: "2026-08-25T10:45:00", Timezone: "Europe/Paris"}, `{"deadline_on":"2026-08-25T10:45:00[Europe/Paris]"}`},
		{"date", "deadline_on", date("2026-08-20"), `{"deadline_on":"2026-08-20[UTC]"}`},
		{"instant", "deadline_on", connector.FieldValue{Kind: "instant", Value: "2026-08-20T09:30:45.120000000Z"}, `{"deadline_on":"2026-08-20T09:30:45.12Z"}`},
		{"clear", "deadline_on", nullValue(), `{"deadline_on":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := fieldCodecs[tc.field].KataPatch(db.Issue{}, tc.value)
			require.NoError(t, err)
			got, err := json.Marshal(patch)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestScheduleCodecValidatesExternalDescriptor(t *testing.T) {
	codec := fieldCodecs["scheduled_on"]
	valid := connector.FieldDescriptor{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"date", "local_datetime", "instant"}, Nullable: true, Writable: true, SchemaRevision: "v1"}
	require.NoError(t, codec.ValidateExternalDescriptor(valid))

	for _, descriptor := range []connector.FieldDescriptor{
		{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true},
		{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"date"}, Nullable: false, Writable: true, SchemaRevision: "v1"},
		{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: false, SchemaRevision: "v1"},
		{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"date", "date"}, Nullable: true, Writable: true, SchemaRevision: "v1"},
		{ID: "field-id", DisplayName: "Plan date", AcceptedKinds: []string{"text"}, Nullable: true, Writable: true, SchemaRevision: "v1"},
	} {
		assert.Error(t, codec.ValidateExternalDescriptor(descriptor))
	}
}
