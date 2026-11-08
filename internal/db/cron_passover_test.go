package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"pgregory.net/rapid"
)

// #nosec G101 -- Portable job UID fixture, not a credential.
const passoverJobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func passoverValue(t interface {
	require.TestingT
	Helper()
}, at time.Time, by string, paused *time.Time) jsontext.Value {
	t.Helper()
	v := map[string]any{"version": 1, "through": at, "by": by}
	if paused != nil {
		v["paused_at"] = *paused
	}
	raw, e := json.Marshal(v)
	require.NoError(t, e)
	return raw
}
func TestCronPassoverDecode(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := passoverValue(t, at, "operator", nil)
	b := passoverValue(t, at.Add(time.Hour), "worker", &at)
	metadata := func(project, job jsontext.Value) JSONBlob {
		m := map[string]jsontext.Value{}
		if project != nil {
			m["cron.passover"] = project
		}
		if job != nil {
			m["cron.passover."+passoverJobUID] = job
		}
		raw, e := json.Marshal(m)
		require.NoError(t, e)
		return JSONBlob(raw)
	}
	require.Nil(t, CronPassoverFromMetadata(`{}`, passoverJobUID))
	for _, tc := range []struct {
		p, j  jsontext.Value
		scope string
		at    time.Time
	}{{a, nil, "project", at}, {a, b, "job", at.Add(time.Hour)}, {b, a, "project", at.Add(time.Hour)}, {a, a, "job", at}} {
		got := CronPassoverFromMetadata(metadata(tc.p, tc.j), passoverJobUID)
		require.NotNil(t, got)
		require.False(t, got.Invalid)
		require.Equal(t, tc.scope, got.Scope)
		require.Equal(t, tc.at, got.Through)
	}
	for _, bad := range []string{`true`, `{}`, `{"version":1,"through":"2026-01-02T03:04:05Z","by":null}`, `{"version":2,"through":"2026-01-02T03:04:05Z","by":"operator"}`, `{"version":1,"through":"2026-01-02T04:04:05+01:00","by":"operator"}`, `{"version":1,"through":"2026-01-02T03:04:05Z","by":" "}`, `{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator","paused_at":null}`, `{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator","extra":true}`} {
		require.True(t, CronPassoverFromMetadata(metadata(jsontext.Value(bad), b), passoverJobUID).Invalid, bad)
		require.True(t, CronPassoverFromMetadata(metadata(a, jsontext.Value(bad)), passoverJobUID).Invalid, bad)
	}
	unrelated := JSONBlob(`{"cron.passover.01ARZ3NDEKTSV4RRFFQ69G5FAW":null}`)
	require.Nil(t, CronPassoverFromMetadata(unrelated, passoverJobUID))
	require.ErrorIs(t, ValidateCronPassoverChange("cron.passover.bad", nil, a), cron.ErrInvalid)
	require.ErrorIs(t, ValidateCronPassoverChange("cron.passover", a, jsontext.Value(`null`)), cron.ErrInvalid)
	require.ErrorIs(t, ValidateCronPassoverChange("cron.passover", b, a), cron.ErrInvalid)
	require.NoError(t, ValidateCronPassoverChange("cron.passover", jsontext.Value(`true`), a))
	require.NoError(t, ValidateCronPassoverChange("cron.passover", nil, a))
	event := RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: "root", Payload: []byte(`{"diff":{"cron.passover":{"from":null,"to":` + string(a) + `}}}`)}
	require.NoError(t, ValidateCronControlEvent(event, "root"))
	require.ErrorIs(t, ValidateCronControlEvent(event, ""), ErrFederationIngestValidation)
}

// Whole-value maximum is associative/commutative/idempotent and preserves the
// winner's attribution, including optional paused_at, across equal timestamps.
func TestCronPassoverFoldProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		base := time.Unix(rapid.Int64Range(-62135596799, 253402214399).Draw(ht, "base"), 0).UTC()
		values := make([]jsontext.Value, 3)
		for i := range values {
			at := base.Add(time.Duration(rapid.IntRange(0, 5).Draw(ht, "at")) * time.Second)
			actor := rapid.String().Draw(ht, "actor")
			if strings.TrimSpace(actor) == "" || len(actor) > 256 {
				actor = "operator"
			}
			var paused *time.Time
			if rapid.Bool().Draw(ht, "paused") {
				paused = &base
			}
			values[i] = passoverValue(ht, at, actor, paused)
		}
		fold := func(a, b jsontext.Value) jsontext.Value {
			v, e := FoldCronPassover(a, b)
			require.NoError(ht, e)
			return v
		}
		a, b, c := values[0], values[1], values[2]
		require.Equal(ht, fold(a, b), fold(b, a))
		require.Equal(ht, fold(fold(a, b), c), fold(a, fold(b, c)))
		require.Equal(ht, fold(a, a), fold(nil, a))
		winner := fold(fold(a, b), c)
		// Independent ordered-maximum oracle, including complete attribution.
		expected := canonicalJSON(values[0])
		var first CronPassover
		require.NoError(ht, json.Unmarshal(values[0], &first))
		expectedTime := first.Through
		for _, raw := range values[1:] {
			var candidate CronPassover
			require.NoError(ht, json.Unmarshal(raw, &candidate))
			canonical := canonicalJSON(raw)
			if candidate.Through.After(expectedTime) || (candidate.Through.Equal(expectedTime) && bytes.Compare(canonical, expected) > 0) {
				expected, expectedTime = canonical, candidate.Through
			}
		}
		require.Equal(ht, expected, winner)
		matched := false
		for _, v := range values {
			if string(canonicalJSON(v)) == string(winner) {
				matched = true
			}
		}
		require.True(ht, matched, "attribution is selected whole, never synthesized")
	})
}

func TestCronPassoverProjection(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	high := passoverValue(t, at.Add(time.Hour), "first", &at)
	low := passoverValue(t, at, "later", nil)
	events := []FoldEvent{{UID: "event1", ProjectUID: "project", Type: "project.metadata_updated", HLCPhysicalMS: 1, Payload: []byte(`{"diff":{"cron.passover":{"from":null,"to":` + string(high) + `},"ordinary":{"from":null,"to":true}}}`)}, {UID: "event2", ProjectUID: "project", Type: "project.metadata_updated", HLCPhysicalMS: 2, Payload: []byte(`{"diff":{"cron.passover":{"from":null,"to":` + string(low) + `}}}`)}}
	for _, order := range [][]int{{0, 1}, {1, 0}, {0, 1, 0, 1}, {1, 1, 0, 0}} {
		incremental := FoldEvents(nil)
		for _, i := range order {
			incremental.apply(events[i])
		}
		var got map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(incremental.ProjectMetadata["project"], &got))
		require.JSONEq(t, string(high), string(got["cron.passover"]), "incremental arrival order must not regress cutoff")
	}
	projection := FoldEvents(events)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(projection.ProjectMetadata["project"], &values))
	require.JSONEq(t, string(high), string(values["cron.passover"]))
	require.Equal(t, "true", string(values["ordinary"]))
	// Federation-enabled baseline metadata uses the same maximum semantics.
	events = append(events, FoldEvent{UID: "baseline", ProjectUID: "project", Type: "project.federation_enabled", HLCPhysicalMS: 3, Payload: []byte(`{"metadata":{"cron.passover":` + string(low) + `,"other":"value"}}`)})
	require.NoError(t, json.Unmarshal(FoldEvents(events).ProjectMetadata["project"], &values))
	require.JSONEq(t, string(high), string(values["cron.passover"]))
}

// The complete-value codec accepts the portable UTC domain and rejects nearby
// malformed representations rather than merely agreeing with its own encoder.
func TestCronPassoverCodecBoundaryProperty(t *testing.T) {
	for _, suffix := range []string{"Z", "+00:00"} {
		for _, paused := range []bool{false, true} {
			fields := map[string]any{"version": 1, "through": "2026-01-02T03:04:05" + suffix, "by": "operator"}
			if paused {
				fields["paused_at"] = "2026-01-02T02:04:05" + suffix
			}
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			_, err = decodeCronPassover(raw)
			require.NoError(t, err, suffix)
			event := controlRecoveryEvent(t, "cron.passover", jsontext.Value("null"), raw, "root", 1)
			require.NoError(t, ValidateCronControlEvent(event, "root"))
			metadata, err := json.Marshal(map[string]jsontext.Value{"cron.passover": raw})
			require.NoError(t, err)
			state := CronPassoverFromMetadata(JSONBlob(metadata), passoverJobUID)
			require.NotNil(t, state)
			require.False(t, state.Invalid)
			anchor := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			_, err = cron.ApplyPassover(cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, state.Through)
			require.NoError(t, err, "zero-offset wire timestamps must work in native planning")
		}
	}
	require.True(t, validCronPassoverTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("zero", 0))))

	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		instant := time.Unix(rapid.Int64Range(-62135596799, 253402300799).Draw(ht, "instant 1"), int64(rapid.IntRange(0, 999999999).Draw(ht, "instant 2"))).UTC()
		actor := strings.Repeat("é", rapid.IntRange(1, 128).Draw(ht, "actor"))
		value := passoverValue(ht, instant, actor, &instant)
		decoded, err := decodeCronPassover(value)
		require.NoError(ht, err)
		require.Equal(ht, CronPassover{Version: 1, Through: instant, By: actor, PausedAt: &instant}, decoded)
		require.NoError(ht, ValidateCronPassoverChange(CronPassoverMetadataPrefix+passoverJobUID, nil, value))
		for _, invalidActor := range []string{"", " \t", strings.Repeat("é", 129)} {
			_, err := decodeCronPassover(passoverValue(ht, instant, invalidActor, nil))
			require.ErrorIs(ht, err, cron.ErrInvalid)
		}
	})
	for _, raw := range []string{
		`{"version":1,"through":"0001-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"through":"0000-12-31T23:59:59Z","by":"operator"}`,
		`{"version":1,"through":"10000-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1.0,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1e0,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"version":1,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z"}`,
		`{"version":1,"through":1,"by":"operator"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":1}`,
	} {
		_, err := decodeCronPassover(jsontext.Value(raw))
		require.ErrorIs(t, err, cron.ErrInvalid, raw)
	}
}

func TestCronPassoverScopeMatrixProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		pKind := rapid.IntRange(0, 3).Draw(ht, "pKind")
		jKind := rapid.IntRange(0, 3).Draw(ht, "jKind")
		pAt := time.Unix(1700000000+int64(rapid.IntRange(0, 3).Draw(ht, "pAt")), 0).UTC()
		jAt := time.Unix(1700000000+int64(rapid.IntRange(0, 3).Draw(ht, "jAt")), 0).UTC()
		values := map[string]jsontext.Value{}
		for _, item := range []struct {
			key  string
			kind int
			at   time.Time
		}{{CronPassoverMetadataKey, pKind, pAt}, {CronPassoverMetadataPrefix + passoverJobUID, jKind, jAt}} {
			switch item.kind {
			case 1:
				values[item.key] = jsontext.Value("null")
			case 2:
				values[item.key] = jsontext.Value(`{"version":1}`)
			case 3:
				values[item.key] = passoverValue(ht, item.at, "operator", nil)
			}
		}
		values[CronPassoverMetadataPrefix+"01ARZ3NDEKTSV4RRFFQ69G5FAW"] = jsontext.Value("null")
		raw, err := json.Marshal(values)
		require.NoError(ht, err)
		got := CronPassoverFromMetadata(JSONBlob(raw), passoverJobUID)
		invalid := (pKind == 2 || jKind == 2)
		if invalid {
			require.Equal(ht, &CronPassoverState{Invalid: true}, got)
			return
		}
		if pKind < 2 && jKind < 2 {
			require.Nil(ht, got)
			return
		}
		want := &CronPassoverState{Through: pAt, Scope: "project"}
		if pKind < 2 || (jKind == 3 && !jAt.Before(pAt)) {
			want = &CronPassoverState{Through: jAt, Scope: "job"}
		}
		require.Equal(ht, want, got, fmt.Sprint(pKind, jKind))
	})
}

// A signed reset may retain malformed history only with the exact later root
// repair and valid current state; changing any witness fails closed.
func TestCronPassoverResetRepairProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		key := CronPassoverMetadataKey
		if rapid.Bool().Draw(ht, "has repair") {
			key = CronPassoverMetadataPrefix + passoverJobUID
		}
		bad, _ := json.Marshal(rapid.String().Draw(ht, "bad"))
		through := time.Unix(rapid.Int64Range(1, 253402300799).Draw(ht, "through"), 0).UTC()
		good := passoverValue(ht, through, "operator", nil)
		makeEvent := func(id string, at int64, from, to jsontext.Value) RemoteEvent {
			raw, err := json.Marshal(map[string]any{"diff": map[string]any{key: map[string]jsontext.Value{"from": from, "to": to}}})
			require.NoError(ht, err)
			return RemoteEvent{EventUID: id, OriginInstanceUID: "root", Type: "project.metadata_updated", HLCPhysicalMS: at, Payload: raw}
		}
		legacy := makeEvent("old", 1, jsontext.Value("null"), bad)
		repair := makeEvent("repair", 2, bad, good)
		baseline := makeEvent("current", 3, jsontext.Value("null"), good)
		hasRepair := rapid.Bool().Draw(ht, "hasRepair")
		exact := rapid.Bool().Draw(ht, "exact")
		later := rapid.Bool().Draw(ht, "later")
		root := rapid.Bool().Draw(ht, "root")
		hasCurrent := rapid.Bool().Draw(ht, "hasCurrent")
		if !exact {
			repair = makeEvent("repair", 2, jsontext.Value("false"), good)
		}
		if !later {
			repair.HLCPhysicalMS = 0
		}
		if !root {
			legacy.OriginInstanceUID = "spoke"
		}
		history := []RemoteEvent{legacy}
		if hasRepair {
			history = append(history, repair)
		}
		var entities []RemoteEvent
		if hasCurrent {
			entities = []RemoteEvent{baseline}
		}
		err := validateCronResetHistory(history, entities, "root")
		if hasRepair && exact && later && root && hasCurrent {
			require.NoError(ht, err)
		} else {
			require.Error(ht, err)
		}
	})
}
