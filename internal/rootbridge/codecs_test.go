package rootbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
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
		patch, err := codec.KataPatch(db.Issue{}, value)
		require.NoError(t, err)
		saved, err := metadata.DecodeSavedSchedule(patch["scheduled_on"])
		require.NoError(t, err)
		assert.Equal(t, value.Value, saved.Value)
		assert.Equal(t, "America/New_York", saved.Timezone)
		assert.NotContains(t, patch, "timezone")
	}
}

func TestScheduleCodecKataPatchKeepsOtherLocalTimezone(t *testing.T) {
	issue := db.Issue{Metadata: db.JSONBlob(`{"deadline_on":"2026-08-20T09:30","timezone":"America/New_York"}`)}
	patch, err := fieldCodecs["scheduled_on"].KataPatch(issue, connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T09:30", Timezone: "America/Los_Angeles"})
	require.NoError(t, err)
	assert.NotContains(t, patch, "deadline_on")
	assert.NotContains(t, patch, "timezone")
}

func TestScheduleCodecKataPatchKeepsOtherDateTimezone(t *testing.T) {
	cases := []struct {
		name     string
		codec    string
		metadata string
	}{
		{
			name:     "scheduled date pins deadline local timezone",
			codec:    "deadline_on",
			metadata: `{"scheduled_on":"2026-08-20","timezone":"America/New_York"}`,
		},
		{
			name:     "deadline date pins scheduled local timezone",
			codec:    "scheduled_on",
			metadata: `{"deadline_on":"2026-08-20","timezone":"America/New_York"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := fieldCodecs[tc.codec].KataPatch(
				db.Issue{Metadata: db.JSONBlob(tc.metadata)},
				connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T09:30", Timezone: "America/Los_Angeles"},
			)
			require.NoError(t, err)
			assert.NotContains(t, patch, "timezone")
		})
	}
}

func TestScheduleCodecKataPatchAdoptsTimezoneWithOtherDateWithoutTimezone(t *testing.T) {
	cases := []struct {
		name     string
		codec    string
		metadata string
	}{
		{
			name:     "scheduled date permits first deadline local timezone",
			codec:    "deadline_on",
			metadata: `{"scheduled_on":"2026-08-20"}`,
		},
		{
			name:     "deadline date permits first scheduled local timezone",
			codec:    "scheduled_on",
			metadata: `{"deadline_on":"2026-08-20"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := fieldCodecs[tc.codec].KataPatch(
				db.Issue{Metadata: db.JSONBlob(tc.metadata)},
				connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T09:30", Timezone: "America/Los_Angeles"},
			)
			require.NoError(t, err)
			saved, err := metadata.DecodeSavedSchedule(patch[tc.codec])
			require.NoError(t, err)
			assert.Equal(t, "2026-08-20T09:30", saved.Value)
			assert.Equal(t, "America/Los_Angeles", saved.Timezone)
			assert.NotContains(t, patch, "timezone")
		})
	}
}

func TestScheduleCodecKataPatchKeepsOtherLegacyLocal(t *testing.T) {
	cases := []struct {
		name     string
		codec    string
		metadata string
	}{
		{
			name:     "scheduled local blocks deadline local timezone",
			codec:    "deadline_on",
			metadata: `{"scheduled_on":"2026-08-20T09:30"}`,
		},
		{
			name:     "deadline local blocks scheduled local timezone",
			codec:    "scheduled_on",
			metadata: `{"deadline_on":"2026-08-20T09:30"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := fieldCodecs[tc.codec].KataPatch(
				db.Issue{Metadata: db.JSONBlob(tc.metadata)},
				connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T10:30", Timezone: "America/Los_Angeles"},
			)
			require.NoError(t, err)
			assert.NotContains(t, patch, "timezone")
		})
	}
}

func TestScheduleCodecKataPatchCanonicalizesAndClears(t *testing.T) {
	codec := fieldCodecs["deadline_on"]
	cases := []struct {
		name  string
		value connector.FieldValue
		want  string
	}{
		{"date", date("2026-08-20"), "2026-08-20"},
		{"instant", connector.FieldValue{Kind: "instant", Value: "2026-08-20T09:30:45.120000000Z"}, "2026-08-20T09:30:45.12Z"},
		{"clear", nullValue(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := codec.KataPatch(db.Issue{}, tc.value)
			require.NoError(t, err)
			if tc.want == "" {
				assert.JSONEq(t, `null`, string(patch["deadline_on"]))
			} else {
				saved, err := metadata.DecodeSavedSchedule(patch["deadline_on"])
				require.NoError(t, err)
				assert.Equal(t, tc.want, saved.Value)
				assert.Equal(t, "UTC", saved.Timezone)
			}
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
