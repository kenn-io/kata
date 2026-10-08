package linearsync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const workspaceID = "11111111-1111-4111-8111-111111111111"
const teamID = "22222222-2222-4222-8222-222222222222"
const projectID = "33333333-3333-4333-8333-333333333333"
const stateID = "44444444-4444-4444-8444-444444444444"
const issueID = "55555555-5555-4555-8555-555555555555"

func testConfig() Config { return Config{WorkspaceID: workspaceID, TeamID: teamID} }

func TestConfigRoundTripAndScope(t *testing.T) {
	c := testConfig()
	c.ProjectID = projectID
	c.StatusSync = "two-way"
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	got, err := DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, "linear:"+workspaceID+"/"+teamID+"/"+projectID, got.SourceKey())
	require.True(t, got.UseTitlePrefix())
	require.Equal(t, "two-way", got.StatusSync)
	for _, raw := range []string{`{}`, `{"workspace_id":"` + workspaceID + `","team_id":"` + teamID + `","token":"secret"}`, `{"workspace_id":"` + workspaceID + `","team_id":"` + teamID + `","status_sync":null}`} {
		_, err := DecodeConfig([]byte(raw))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
	c.StatusSync = "wrong"
	_, err = EncodeConfig(c)
	require.Error(t, err)
	for _, since := range []string{"bad", "0000-01-01", "2026-01-01T00:00:00.1Z"} {
		_, err := ParseSince(since)
		require.Error(t, err)
	}
	at, err := ParseSince("2026-01-01")
	require.NoError(t, err)
	require.Equal(t, "2026-01-01T00:00:00Z", at.Format("2006-01-02T15:04:05Z07:00"))
}

// UUID canonicalization must accept exactly the documented UUID spellings and
// reject arbitrary path/URL inputs. Built-in fuzzing exercises both directions.
func FuzzCanonicalID(f *testing.F) {
	for _, s := range []string{teamID, strings.ToUpper(teamID), strings.ReplaceAll(teamID, "-", ""), "", "../issue", "https://linear.app/issue/EX-1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := CanonicalID(s)
		compact := strings.ReplaceAll(s, "-", "")
		valid := len(s) == 32 || len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
		valid = valid && len(compact) == 32
		for _, r := range compact {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				valid = false
			}
		}
		if !valid {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		expected := strings.ToLower(compact[:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:])
		require.Equal(t, expected, got)
	})
}
