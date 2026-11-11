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
	_, err := codec.KataPatch(db.Issue{Metadata: db.JSONBlob(`{"timezone":"Local"}`)}, connector.FieldValue{Kind: fieldKindLocalDateTime, Value: "2026-08-21T09:30", Timezone: "Local"})
	require.ErrorContains(t, err, "unknown schedule timezone")
}

func TestScheduleCodecRecurrenceZonePrecedence(t *testing.T) {
	saved, err := metadata.NewScheduleValue("2026-08-20", "Europe/Paris")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, field, raw, want string
		value                  connector.FieldValue
	}{
		{"legacy", "scheduled_on", `{"scheduled_on":"2026-08-20"}`, "America/New_York", date("2026-08-21")},
		{"saved", "scheduled_on", `{"scheduled_on":` + string(saved) + `}`, "Europe/Paris", date("2026-08-21")},
		{"issue zone", "scheduled_on", `{"timezone":"Europe/Paris"}`, "Europe/Paris", date("2026-08-21")},
		{"deadline", "deadline_on", `{}`, "UTC", date("2026-08-21")},
		{"connector zone", "scheduled_on", `{}`, "Asia/Tokyo", connector.FieldValue{Kind: fieldKindLocalDateTime, Value: "2026-08-21T09:30", Timezone: "Asia/Tokyo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codec := scheduleCodec{key: tc.field, defaultTimezone: "UTC"}
			recurrenceZone := ""
			if tc.field == "scheduled_on" {
				recurrenceZone = "America/New_York"
			}
			patch, err := codec.kataPatch(db.Issue{Metadata: db.JSONBlob(tc.raw)}, tc.value, recurrenceZone)
			require.NoError(t, err)
			got, err := metadata.DecodeSavedSchedule(patch[tc.field])
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Timezone)
		})
	}
}

func TestScheduleCodecPreservesAliasZone(t *testing.T) {
	original, err := metadata.NewScheduleValue("2026-08-20T09:30", "US/Eastern")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, metadata, value, kind string
	}{
		{"existing date", `{"scheduled_on":` + string(original) + `}`, "2026-08-20T10:30", "local_datetime"},
		{"first instant", `{"timezone":"US/Eastern","deadline_on":"2026-11-30"}`, "2026-11-12T02:30:00Z", "instant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := connector.FieldValue{Kind: tc.kind, Value: tc.value}
			if tc.kind == "local_datetime" {
				value.Timezone = "US/Eastern"
			}
			patch, err := fieldCodecs["scheduled_on"].KataPatch(db.Issue{Metadata: db.JSONBlob(tc.metadata)}, value)
			require.NoError(t, err)
			require.Len(t, patch, 1)
			saved, err := metadata.DecodeSavedSchedule(patch["scheduled_on"])
			require.NoError(t, err)
			require.Equal(t, &metadata.SavedSchedule{Value: tc.value, Timezone: "US/Eastern", TZDataVersion: metadata.ScheduleTZDataVersion}, saved)
		})
	}
}

func TestScheduleCodecKataPatchKeepsOtherSchedule(t *testing.T) {
	for _, tc := range []struct {
		name, codec, metadata string
	}{
		{"other local with zone", "scheduled_on", `{"deadline_on":"2026-08-20T09:30","timezone":"America/New_York"}`},
		{"scheduled local without zone", "deadline_on", `{"scheduled_on":"2026-08-20T09:30"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := fieldCodecs[tc.codec].KataPatch(
				db.Issue{Metadata: db.JSONBlob(tc.metadata)},
				connector.FieldValue{Kind: "local_datetime", Value: "2026-08-20T10:30", Timezone: "America/Los_Angeles"},
			)
			require.NoError(t, err)
			require.Len(t, patch, 1)
			saved, err := metadata.DecodeSavedSchedule(patch[tc.codec])
			require.NoError(t, err)
			require.NotNil(t, saved)
			assert.Equal(t, "2026-08-20T10:30", saved.Value)
			assert.Equal(t, "America/Los_Angeles", saved.Timezone)
		})
	}
}

func TestScheduleCodecKataPatchCanonicalizesAndClears(t *testing.T) {
	codec := fieldCodecs["deadline_on"]
	cases := []struct {
		name  string
		value connector.FieldValue
		want  string
		clear bool
	}{
		{"date", date("2026-08-20"), "2026-08-20", false},
		{"instant", connector.FieldValue{Kind: "instant", Value: "2026-08-20T09:30:45.120000000Z"}, "2026-08-20T09:30:45.12Z", false},
		{"clear", nullValue(), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := codec.KataPatch(db.Issue{}, tc.value)
			require.NoError(t, err)
			if tc.clear {
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
