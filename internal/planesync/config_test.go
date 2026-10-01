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
