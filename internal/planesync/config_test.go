package planesync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testProjectID = "11111111-1111-4111-8111-111111111111"
const testItemID = "22222222-2222-4222-8222-222222222222"
const testStateID = "33333333-3333-4333-8333-333333333333"
const testUserID = "44444444-4444-4444-8444-444444444444"

func testConfig() Config {
	return Config{APIOrigin: "https://api.plane.so", WebOrigin: "https://app.plane.so", Workspace: "example-workspace", ProjectID: testProjectID}
}

func TestConfigCanonicalIdentityAndPresence(t *testing.T) {
	c := testConfig()
	c.ProjectID = strings.ToUpper(strings.ReplaceAll(testProjectID, "-", ""))
	c.APIOrigin = "https://API.plane.so:443/"
	c.Since = "2026-09-01T02:00:00+02:00"
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	got, err := DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, testProjectID, got.ProjectID)
	require.Equal(t, "https://api.plane.so", got.APIOrigin)
	require.Equal(t, "2026-09-01T00:00:00Z", got.Since)
	require.True(t, got.UseTitlePrefix())
	require.Equal(t, "plane:https://api.plane.so/example-workspace/"+testProjectID, got.SourceKey())
	require.Equal(t, "example-workspace/"+testProjectID, got.RemoteID())
	got.TitlePrefix = new(false)
	raw, err = EncodeConfig(got)
	require.NoError(t, err)
	got, err = DecodeConfig(raw)
	require.NoError(t, err)
	require.False(t, got.UseTitlePrefix())
	for _, value := range []string{"null", "{}", "{\"token\":\"secret\"}", strings.TrimSuffix(string(raw), "}") + ",\"token\":\"secret\"}", strings.Replace(string(raw), "\"title_prefix\":false", "\"title_prefix\":null", 1)} {
		_, err := DecodeConfig([]byte(value))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestConfigRejectsUnsafeIdentity(t *testing.T) {
	for _, slug := range []string{"", ".", "..", "../elsewhere", "example/workspace", "example?key", "%2e%2e", "x#y", "x\ny", " x "} {
		c := testConfig()
		c.Workspace = slug
		_, err := EncodeConfig(c)
		require.Error(t, err, slug)
	}
	for _, id := range []string{"", "not-a-uuid", "urn:uuid:" + testProjectID, "{" + testProjectID + "}", "../" + testProjectID} {
		_, err := CanonicalID(id)
		require.Error(t, err)
	}
	for _, since := range []string{"2026-09-01T00:00:00.123Z", "yesterday", "0000-01-01", "0001-01-01T00:00:00+01:00"} {
		_, err := ParseSince(since)
		require.Error(t, err)
	}
	cutoff, err := ParseSince("2026-09-01")
	require.NoError(t, err)
	require.Equal(t, "2026-09-01T00:00:00Z", cutoff.Format("2006-01-02T15:04:05Z07:00"))
	cutoff, err = ParseSince("")
	require.NoError(t, err)
	require.Nil(t, cutoff)
}

func TestStatusConfigSupportsModeTargetsAndPrivateCheckpoint(t *testing.T) {
	raw := `{"api_origin":"https://api.plane.so","web_origin":"https://app.plane.so","workspace":"example-workspace","project_id":"11111111-1111-4111-8111-111111111111","status_sync":"two-way","closed_state_id":"55555555-5555-4555-8555-555555555555","open_state_id":"33333333-3333-4333-8333-333333333333","_status_sync":{"pending":{"after":2,"through":7}}}`
	c, err := DecodeConfig([]byte(raw))
	require.NoError(t, err)
	encoded, err := EncodeConfig(c)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"status_sync":"two-way"`)
	require.Contains(t, string(encoded), `"closed_state_id":"55555555-5555-4555-8555-555555555555"`)
	require.Contains(t, string(encoded), `"open_state_id":"33333333-3333-4333-8333-333333333333"`)
	require.NotContains(t, string(encoded), "_status_sync")
	for _, key := range []string{"closed_state_id", "open_state_id"} {
		original := `"closed_state_id":"55555555-5555-4555-8555-555555555555"`
		expected := "abcdefab-cdef-4abc-8def-abcdefabcdef"
		canonicalInput := strings.Replace(raw, original, `"`+key+`":"ABCDEFABCDEF4ABC8DEFABCDEFABCDEF"`, 1)
		if key == "open_state_id" {
			canonicalInput = strings.Replace(raw, `"open_state_id":"33333333-3333-4333-8333-333333333333"`, `"open_state_id":"ABCDEFABCDEF4ABC8DEFABCDEFABCDEF"`, 1)
		}
		decoded, err := DecodeConfig([]byte(canonicalInput))
		require.NoError(t, err)
		target := decoded.ClosedStateID
		if key == "open_state_id" {
			target = decoded.OpenStateID
		}
		require.Equal(t, expected, target)
		invalid := strings.Replace(canonicalInput, `"ABCDEFABCDEF4ABC8DEFABCDEFABCDEF"`, `"../invalid"`, 1)
		_, err = DecodeConfig([]byte(invalid))
		require.Error(t, err)
	}

	for _, mode := range []string{"none", "", "null"} {
		bad := strings.Replace(raw, `"two-way"`, `"`+mode+`"`, 1)
		if mode == "null" {
			bad = strings.Replace(raw, `"two-way"`, `null`, 1)
		}
		_, err := DecodeConfig([]byte(bad))
		require.Error(t, err)
	}
}
