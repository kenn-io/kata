package db

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileEvidenceReplayValidation(t *testing.T) {
	valid := `{"evidence":[{"type":"file","file":{"source":"notes","path":"report.md","origin":"origin","size":0,"sha256":"` + strings.Repeat("0", 64) + `"}}]}`
	require.NoError(t, ValidateFederationEntries("issue.closed", "event", jsontext.Value(valid)))
	for _, bad := range []string{
		strings.Replace(valid, `"size":0,`, "", 1),
		strings.Replace(valid, `"origin":"origin",`, "", 1),
		strings.Replace(valid, `"size":0`, `"size":-1`, 1),
		strings.Replace(valid, `"report.md"`, `"../report.md"`, 1),
		strings.Replace(valid, strings.Repeat("0", 64), "not-sha256", 1),
		`{"evidence":[{"type":"file"}]}`,
	} {
		require.Error(t, ValidateFederationEntries("issue.closed", "event", jsontext.Value(bad)), bad)
	}
	require.NoError(t, ValidateFederationEntries("issue.closed", "event", jsontext.Value(`{"evidence":[{"type":"legacy"}]}`)), "do not apply today's close rules to old events")
}
