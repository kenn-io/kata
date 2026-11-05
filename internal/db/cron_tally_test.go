package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const tallyTestUID = "01K00000000000000000000001"
const tallyTestKey = "cron.tally." + tallyTestUID

func tallyTestEntry(kind, count string) string {
	return fmt.Sprintf(`{"kind":%q,"uid":%q,"coverage_started_at":"2026-08-01T00:00:00Z","last_pruned_at":"2026-08-02T00:00:00Z","oldest_created_at":"2025-01-01T00:00:00Z","newest_created_at":"2025-12-31T00:00:00Z","counts":{"succeeded":%s,"failed":0,"cancelled":0,"unknown":0}}`, kind, tallyTestUID, count)
}

func tallyTestMetadata(value string) JSONBlob {
	return JSONBlob(fmt.Sprintf(`{%q:%s,"ordinary":{"keep":true}}`, tallyTestKey, value))
}

func TestCronTallyCodec(t *testing.T) {
	entry := tallyTestEntry("job", "9007199254740993")
	value := `{"version":1,"groups":{"job":` + entry + `}}`
	// All four MaxInt64 statuses are valid: positivity cannot sum into int64.
	_, err := DecodeCronTallies(tallyTestMetadata(strings.ReplaceAll(strings.ReplaceAll(value, "9007199254740993", "9223372036854775807"), `":0`, `":9223372036854775807`)))
	require.NoError(t, err)
	_, err = DecodeCronTallies(tallyTestMetadata(strings.ReplaceAll(strings.ReplaceAll(value, "2026-08-01", "0001-01-01"), "2025-01-01", "0001-01-01")))
	require.NoError(t, err, "year one is within the supported date domain")
	invalid := map[string]string{
		"missing status":    strings.Replace(value, `,"unknown":0`, "", 1),
		"extra status":      strings.Replace(value, `"unknown":0`, `"unknown":0,"running":1`, 1),
		"null status":       strings.Replace(value, `"failed":0`, `"failed":null`, 1),
		"negative":          strings.Replace(value, "9007199254740993", "-1", 1),
		"zero total":        strings.Replace(value, "9007199254740993", "0", 1),
		"fraction":          strings.Replace(value, "9007199254740993", "1.5", 1),
		"exponent":          strings.Replace(value, "9007199254740993", "1e2", 1),
		"overflow":          strings.Replace(value, "9007199254740993", "9223372036854775808", 1),
		"string count":      strings.Replace(value, "9007199254740993", `"1"`, 1),
		"unknown version":   strings.Replace(value, `"version":1`, `"version":2`, 1),
		"missing version":   strings.Replace(value, `"version":1,`, "", 1),
		"null version":      strings.Replace(value, `"version":1`, `"version":null`, 1),
		"empty groups":      `{"version":1,"groups":{}}`,
		"null groups":       `{"version":1,"groups":null}`,
		"null group":        `{"version":1,"groups":{"job":null}}`,
		"null value":        "null",
		"extra member":      strings.Replace(value, `"version":1`, `"extra":true,"version":1`, 1),
		"missing kind":      strings.Replace(value, `"kind":"job",`, "", 1),
		"wrong kind":        strings.Replace(value, `"kind":"job"`, `"kind":"workflow"`, 1),
		"unknown kind":      strings.Replace(value, `"groups":{"job"`, `"groups":{"other"`, 1),
		"wrong uid":         strings.Replace(value, `"uid":"`+tallyTestUID+`"`, `"uid":"01K00000000000000000000002"`, 1),
		"duplicate":         strings.Replace(value, `"version":1`, `"version":1,"version":1`, 1),
		"null date":         strings.Replace(value, `"2026-08-01T00:00:00Z"`, "null", 1),
		"invalid date":      strings.Replace(value, "2026-08-01T00:00:00Z", "2026-02-30T00:00:00Z", 1),
		"coverage reversed": strings.Replace(value, "2026-08-01T00:00:00Z", "2026-08-03T00:00:00Z", 1),
		"range reversed":    strings.Replace(value, "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 1),
	}
	for name, bad := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeCronTallies(tallyTestMetadata(bad))
			require.Error(t, err)
		})
	}
	for _, key := range []string{"cron.tally.", "cron.tally.not-a-uid", "cron.tally." + strings.ToLower(tallyTestUID)} {
		_, err := DecodeCronTallies(JSONBlob(fmt.Sprintf(`{%q:%s}`, key, value)))
		require.Error(t, err, key)
	}
}

func TestCronTallyExactIntegers(t *testing.T) {
	check := func(ht require.TestingT, count int64) {
		literal := fmt.Sprint(count)
		value := `{"version":1,"groups":{"job":` + tallyTestEntry("job", literal) + `}}`
		decoded, err := DecodeCronTallies(tallyTestMetadata(value))
		require.NoError(ht, err)
		require.Equal(ht, count, decoded[tallyTestUID].Groups["job"].Counts.Succeeded)
		encoded, err := json.Marshal(decoded[tallyTestUID])
		require.NoError(ht, err)
		require.Contains(ht, string(encoded), `"succeeded":`+literal)
	}
	check(t, 9007199254740993)
	check(t, math.MaxInt64)
	hegel.Test(t, func(ht *hegel.T) {
		check(ht, hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64)))
	}, hegel.WithTestCases(1000))
}

func TestCronTallyUnionAndRootPreservation(t *testing.T) {
	job := `{"version":1,"groups":{"job":` + tallyTestEntry("job", "3") + `}}`
	workflow := `{"version":1,"groups":{"workflow":` + tallyTestEntry("workflow", "5") + `}}`
	patch, err := MergeCronTallyMetadata(tallyTestMetadata(job), tallyTestMetadata(workflow))
	require.NoError(t, err)
	merged, err := ApplyMetadataPatch([]byte(tallyTestMetadata(job)), patch)
	require.NoError(t, err)
	values, err := DecodeCronTallies(JSONBlob(merged))
	require.NoError(t, err)
	require.Len(t, values[tallyTestUID].Groups, 2)
	require.Equal(t, int64(3), values[tallyTestUID].Groups["job"].Counts.Succeeded)
	require.Equal(t, int64(5), values[tallyTestUID].Groups["workflow"].Counts.Succeeded)
	// Equal timestamp instants with different zone spellings do not conflict.
	equal := strings.Replace(job, "2026-08-01T00:00:00Z", "2026-08-01T01:00:00+01:00", 1)
	patch, err = MergeCronTallyMetadata(tallyTestMetadata(job), tallyTestMetadata(equal))
	require.NoError(t, err)
	require.Empty(t, patch)
	_, err = MergeCronTallyMetadata(tallyTestMetadata(job), tallyTestMetadata(strings.Replace(job, `"succeeded":3`, `"succeeded":4`, 1)))
	require.ErrorIs(t, err, ErrCronTallyConflict)
	for _, current := range []JSONBlob{tallyTestMetadata(job), `{}`} {
		preserved, err := PreserveCronTallies(current, tallyTestMetadata(workflow))
		require.NoError(t, err)
		values, err := DecodeCronTallies(preserved)
		require.NoError(t, err)
		if current == `{}` {
			require.Empty(t, values)
		} else {
			require.Len(t, values[tallyTestUID].Groups, 1)
			require.Equal(t, int64(3), values[tallyTestUID].Groups["job"].Counts.Succeeded)
		}
		require.Contains(t, string(preserved), `"ordinary":{"keep":true}`)
	}
	_, err = PreserveCronTallies(tallyTestMetadata("null"), `{}`)
	require.Error(t, err)
}

func TestCronTallyEventsAndGenericPatch(t *testing.T) {
	value := `{"version":1,"groups":{"job":` + tallyTestEntry("job", "1") + `}}`
	for _, raw := range []jsontext.Value{[]byte(value), []byte("null")} {
		for _, key := range []string{tallyTestKey, "cron.tally.malformed", "cron.tally."} {
			require.Error(t, RejectCronTallyPatch(map[string]jsontext.Value{key: raw, "ordinary": []byte("1")}))
		}
	}
	require.NoError(t, RejectCronTallyPatch(map[string]jsontext.Value{"ordinary": []byte("1")}))
	for _, sides := range [][2]string{{"null", value}, {value, "null"}, {value, value}} {
		event := RemoteEvent{Type: "project.metadata_updated", Payload: []byte(fmt.Sprintf(`{"diff":{%q:{"from":%s,"to":%s}}}`, tallyTestKey, sides[0], sides[1]))}
		require.NoError(t, ValidateCronTallyEvent(event))
	}
	for _, typ := range []string{"project.metadata_updated", "project.federation_enabled"} {
		payload := fmt.Sprintf(`{"metadata":%s}`, tallyTestMetadata(value))
		if typ == "project.metadata_updated" {
			payload = fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, tallyTestKey, value)
		}
		require.NoError(t, ValidateCronTallyEvent(RemoteEvent{Type: typ, Payload: []byte(payload)}))
		require.Error(t, ValidateCronTallyEvent(RemoteEvent{Type: typ, Payload: []byte(strings.Replace(payload, `"failed":0`, `"failed":null`, 1))}))
	}
	// Validate the historical from side even when the final to side is valid.
	event := RemoteEvent{Type: "project.metadata_updated", Payload: []byte(fmt.Sprintf(`{"diff":{%q:{"from":{"version":1},"to":%s}}}`, tallyTestKey, value))}
	require.Error(t, ValidateCronTallyEvent(event))
}

func TestCronTallyProjectFoldIsAbsolute(t *testing.T) {
	job := `{"version":1,"groups":{"job":` + tallyTestEntry("job", "3") + `}}`
	both := `{"version":1,"groups":{"job":` + tallyTestEntry("job", "2") + `,"workflow":` + tallyTestEntry("workflow", "5") + `}}`
	for _, current := range []jsontext.Value{[]byte(`{}`), []byte(tallyTestMetadata(both))} {
		diff := []byte(fmt.Sprintf(`{%q:{"from":%s,"to":%s}}`, tallyTestKey, strings.Replace(job, `"succeeded":3`, `"succeeded":2`, 1), job))
		fold := FoldEvents([]FoldEvent{
			{ProjectUID: "project", Type: "project.federation_enabled", Payload: []byte(fmt.Sprintf(`{"metadata":%s}`, current)), HLCPhysicalMS: 1},
			{ProjectUID: "project", Type: "project.metadata_updated", Payload: []byte(fmt.Sprintf(`{"diff":%s}`, diff)), HLCPhysicalMS: 2},
		})
		values, err := DecodeCronTallies(JSONBlob(fold.ProjectMetadata["project"]))
		require.NoError(t, err)
		require.Len(t, values[tallyTestUID].Groups, 1)
		require.Equal(t, int64(3), values[tallyTestUID].Groups["job"].Counts.Succeeded)
	}
}

func TestCronTallyImportAndBaselineValidation(t *testing.T) {
	value := `{"version":1,"groups":{"job":` + tallyTestEntry("job", "9007199254740993") + `}}`
	valid := tallyTestMetadata(value)
	bad := tallyTestMetadata(strings.Replace(value, `"failed":0`, `"failed":null`, 1))
	require.NoError(t, ValidateImportRecords([]ImportRecord{&ProjectExport{Metadata: []byte(valid)}}))
	require.Error(t, ValidateImportRecords([]ImportRecord{&ProjectExport{Metadata: []byte(bad)}}))
	for _, typ := range []string{"project.metadata_updated", "project.federation_enabled"} {
		payload := fmt.Sprintf(`{"metadata":%s}`, bad)
		if typ == "project.metadata_updated" {
			payload = fmt.Sprintf(`{"diff":{%q:{"from":%s,"to":%s}}}`, tallyTestKey, strings.Replace(value, `"failed":0`, `"failed":null`, 1), value)
		}
		require.Error(t, ValidateFederationEntries(typ, "event", []byte(payload)))
		require.Error(t, ValidateImportRecords([]ImportRecord{&EventExport{Type: typ, Payload: []byte(payload)}}))
	}
	payload, err := ProjectMetadataAdoptionPayload(valid)
	require.NoError(t, err)
	require.Contains(t, payload, `"succeeded":9007199254740993`)
	_, err = ProjectMetadataAdoptionPayload(bad)
	require.Error(t, err)
}

// Accepted timestamps must remain in the documented year domain after the
// decoder's UTC normalization and the next backup/counts serialization.
func TestCronTallyTimestampDomainIsUTCClosed(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		offsetMinutes := hegel.Draw(ht, hegel.Integers(-1439, 1439))
		upper := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		at := time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", offsetMinutes*60))
		if upper {
			at = time.Date(9999, 12, 31, 23, 59, 59, 999999999, at.Location())
		}
		count := hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64))
		entry := CronTallyEntry{Kind: "job", UID: tallyTestUID, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: CronTallyCounts{Succeeded: count}}
		raw, err := json.Marshal(map[string]CronTallyValue{tallyTestKey: {Version: 1, Groups: map[string]CronTallyEntry{"job": entry}}})
		require.NoError(ht, err)
		values, err := DecodeCronTallies(JSONBlob(raw))
		if at.UTC().Year() < 0 || at.UTC().Year() > 9999 {
			require.Error(ht, err, "normalization must not create an out-of-domain instant")
			return
		}
		require.NoError(ht, err)
		normalized := values[tallyTestUID].Groups["job"]
		require.True(ht, normalized.CoverageStartedAt.Equal(at))
		require.Equal(ht, count, normalized.Counts.Succeeded)
		raw, err = json.Marshal(map[string]CronTallyValue{tallyTestKey: values[tallyTestUID]})
		require.NoError(ht, err)
		roundTrip, err := DecodeCronTallies(JSONBlob(raw))
		require.NoError(ht, err)
		require.Equal(ht, values, roundTrip)
	}, hegel.WithTestCases(400))
}
