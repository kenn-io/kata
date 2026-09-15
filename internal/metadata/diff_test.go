package metadata

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeJSONPreservesFingerprintKeyOrder(t *testing.T) {
	// Persisted fingerprints use Go string ordering, including keys whose
	// UTF-8 and UTF-16 order differ. Numbers must retain their exact text.
	raw := jsontext.Value(`{"😀":9007199254740993,"\ue000":1.0}`)
	assert.Equal(t, "{\"\ue000\":1.0,\"😀\":9007199254740993}", string(NormalizeJSON(raw)))
}

func TestDiffAddedKey(t *testing.T) {
	old := jsontext.Value(`{}`)
	newBlob := jsontext.Value(`{"scheduled_on":"2026-06-01"}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "scheduled_on")
	assert.Nil(t, d["scheduled_on"].From)
	assert.JSONEq(t, `"2026-06-01"`, string(d["scheduled_on"].To))
}

func TestDiffRemovedKey(t *testing.T) {
	old := jsontext.Value(`{"scheduled_on":"2026-05-01"}`)
	newBlob := jsontext.Value(`{}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "scheduled_on")
	assert.JSONEq(t, `"2026-05-01"`, string(d["scheduled_on"].From))
	assert.Nil(t, d["scheduled_on"].To)
}

func TestDiffChangedKey(t *testing.T) {
	old := jsontext.Value(`{"scheduled_on":"2026-05-01"}`)
	newBlob := jsontext.Value(`{"scheduled_on":"2026-06-15"}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "scheduled_on")
	assert.JSONEq(t, `"2026-05-01"`, string(d["scheduled_on"].From))
	assert.JSONEq(t, `"2026-06-15"`, string(d["scheduled_on"].To))
}

func TestDiffUnchangedKeySuppressed(t *testing.T) {
	blob := jsontext.Value(`{"scheduled_on":"2026-05-01"}`)
	d, err := Diff(blob, blob)
	require.NoError(t, err)
	assert.Empty(t, d, "identical blobs should produce an empty diff")
}

func TestDiffNullClearsKey(t *testing.T) {
	old := jsontext.Value(`{"scheduled_on":"2026-05-01"}`)
	newBlob := jsontext.Value(`{"scheduled_on":null}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "scheduled_on")
	assert.JSONEq(t, `"2026-05-01"`, string(d["scheduled_on"].From))
	assert.Nil(t, d["scheduled_on"].To)
}

func TestDiffNullToNullNoOp(t *testing.T) {
	old := jsontext.Value(`{"scheduled_on":null}`)
	newBlob := jsontext.Value(`{"scheduled_on":null}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	assert.Empty(t, d)
}

func TestDiffAbsentToNullNoOp(t *testing.T) {
	old := jsontext.Value(`{}`)
	newBlob := jsontext.Value(`{"scheduled_on":null}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	assert.Empty(t, d)
}

func TestDiffEmptyBlobsNoOp(t *testing.T) {
	d, err := Diff(jsontext.Value(`null`), jsontext.Value(`{}`))
	require.NoError(t, err)
	assert.Empty(t, d)
}

func TestDiff_NullToValueNormalizesFromAsNil(t *testing.T) {
	old := jsontext.Value(`{"k":null}`)
	newBlob := jsontext.Value(`{"k":"v"}`)
	diff, err := Diff(old, newBlob)
	require.NoError(t, err)
	kd, ok := diff["k"]
	require.True(t, ok)
	assert.Nil(t, kd.From, "From must be nil when the prior value was JSON null")
	assert.JSONEq(t, `"v"`, string(kd.To))
}

// TestDiffSurfacesUnknownKey pins that Diff operates on the JSON object, not
// the registry: opaque keys (not in IssueRegistry / ProjectRegistry) still
// show up in the per-key diff. This is what lets consumers carry their own
// metadata and still see change events for it.
func TestDiffSurfacesUnknownKey(t *testing.T) {
	old := jsontext.Value(`{}`)
	newBlob := jsontext.Value(`{"definitely_not_a_key":"yellow"}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "definitely_not_a_key",
		"Diff must surface unknown keys; it works at the JSON layer, not the registry")
	assert.Nil(t, d["definitely_not_a_key"].From)
	assert.JSONEq(t, `"yellow"`, string(d["definitely_not_a_key"].To))
}

// TestDiff_LargeIntValuesDistinct pins that two distinct integers larger than
// 2^53 are reported as a change rather than collapsing to the same float64.
// Before NormalizeJSON used a UseNumber decoder, both sides normalized to the
// same imprecise float and the change was silently suppressed.
func TestDiff_LargeIntValuesDistinct(t *testing.T) {
	old := jsontext.Value(`{"k":9223372036854775807}`)
	newBlob := jsontext.Value(`{"k":9223372036854775806}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)
	require.Contains(t, d, "k",
		"two distinct large integers must be reported as a change, not collapsed via float64")
	assert.Equal(t, `9223372036854775807`, string(d["k"].From))
	assert.Equal(t, `9223372036854775806`, string(d["k"].To))
}

func TestDiffMultipleKeys(t *testing.T) {
	old := jsontext.Value(`{"scheduled_on":"2026-05-01","someday":true}`)
	newBlob := jsontext.Value(`{"scheduled_on":"2026-06-01","deadline_on":"2026-07-01"}`)
	d, err := Diff(old, newBlob)
	require.NoError(t, err)

	// scheduled_on changed
	require.Contains(t, d, "scheduled_on")
	assert.JSONEq(t, `"2026-05-01"`, string(d["scheduled_on"].From))
	assert.JSONEq(t, `"2026-06-01"`, string(d["scheduled_on"].To))

	// someday removed
	require.Contains(t, d, "someday")
	assert.JSONEq(t, `true`, string(d["someday"].From))
	assert.Nil(t, d["someday"].To)

	// deadline_on added
	require.Contains(t, d, "deadline_on")
	assert.Nil(t, d["deadline_on"].From)
	assert.JSONEq(t, `"2026-07-01"`, string(d["deadline_on"].To))
}
