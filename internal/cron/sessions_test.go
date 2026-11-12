package cron

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummarySessionsContract(t *testing.T) {
	session := `{"agent":"claude","session_id":"00000000-0000-4000-8000-000000000001","step":"main"}`
	sessions := func(n int) string {
		var entries []string
		for i := range n {
			entries = append(entries, fmt.Sprintf(`{"agent":"claude","session_id":"00000000-0000-4000-8000-%012d"}`, i))
		}
		return strings.Join(entries, ",")
	}
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"legacy", `{"version":1}`, true},
		{"session", `{"version":1,"sessions":[` + session + `]}`, true},
		{"incomplete", `{"version":1,"sessions_incomplete":true}`, true},
		{"false", `{"version":1,"sessions_incomplete":false}`, false},
		{"null incomplete", `{"version":1,"sessions_incomplete":null}`, false},
		{"empty", `{"version":1,"sessions":[]}`, false},
		{"null", `{"version":1,"sessions":null}`, false},
		{"invalid id", `{"version":1,"sessions":[{"agent":"claude","session_id":"bad"}]}`, false},
		{"invalid agent", `{"version":1,"sessions":[{"agent":"CLAUDE","session_id":"bad"}]}`, false},
		{"empty step", `{"version":1,"sessions":[{"agent":"codex","session_id":"00000000-0000-4000-8000-000000000001","step":""}]}`, false},
		{"null step", `{"version":1,"sessions":[{"agent":"codex","session_id":"00000000-0000-4000-8000-000000000001","step":null}]}`, false},
		{"invalid step", `{"version":1,"sessions":[{"agent":"codex","session_id":"00000000-0000-4000-8000-000000000001","step":"a/b"}]}`, false},
		{"duplicate", `{"version":1,"sessions":[` + session + `,` + session + `]}`, false},
		{"maximum", `{"version":1,"sessions":[` + sessions(64) + `]}`, true},
		{"too many", `{"version":1,"sessions":[` + sessions(65) + `]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSummary([]byte(tc.input))
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestLegacySummaryEncodingUnchanged(t *testing.T) {
	raw, err := json.Marshal(Summary{Version: 1})
	require.NoError(t, err)
	require.Equal(t, `{"version":1}`, string(raw))
}

func TestSummaryGoValuesPreserveInvalidSessionFields(t *testing.T) {
	emptySessions := []Session{}
	falseValue := false
	emptyStep := ""
	withEmptyStep := []Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001", Step: &emptyStep}}
	for name, summary := range map[string]Summary{
		"empty sessions":   {Version: 1, Sessions: &emptySessions},
		"false incomplete": {Version: 1, SessionsIncomplete: &falseValue},
		"empty step":       {Version: 1, Sessions: &withEmptyStep},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(summary)
			require.NoError(t, err)
			_, err = ParseSummary(raw)
			require.Error(t, err)
		})
	}
}
