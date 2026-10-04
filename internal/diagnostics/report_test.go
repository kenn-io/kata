package diagnostics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReportCountsAndPanicIsolation(t *testing.T) {
	checks := []Check{
		Run("broken", "config", func() Check { panic("secret-token") }),
		Run("later", "daemon", func() Check { return Check{Status: "ok", Summary: "later check ran"} }),
		{ID: "warning", Category: "config", Status: "warn"},
		{ID: "disabled", Category: "integration", Status: "info"},
	}
	report := NewReport(checks)
	require.Equal(t, 1, report.Summary.Fail)
	require.Equal(t, 1, report.Summary.Warn)
	require.Equal(t, 1, report.Summary.OK)
	require.Equal(t, 1, report.Summary.Info)
	require.True(t, report.Failed())
	data, err := json.Marshal(report)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(data), "secret-token"))
	require.Equal(t, "later", report.Checks[1].ID)
	require.False(t, NewReport(checks[1:]).Failed())
}

func TestEmptyReportEncodesChecksAsArray(t *testing.T) {
	data, err := json.Marshal(NewReport(nil))
	require.NoError(t, err)
	require.Contains(t, string(data), `"checks":[]`)
}

func TestReportCountsInvalidStatusesAsFailures(t *testing.T) {
	for _, status := range []Status{"", "typo"} {
		report := NewReport([]Check{{ID: "example", Status: status}})
		require.True(t, report.Failed(), "an invalid check must not disappear from the summary")
		require.Equal(t, 1, report.Summary.Fail)
		require.Equal(t, StatusFail, report.Checks[0].Status)
		require.Equal(t, "example", report.Checks[0].ID)
	}
}

// The public JSON contract promises arrays even when there are no findings or
// details. Vary every status and report size, including the empty report.
func FuzzReportJSONArrays(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 255})
	f.Fuzz(func(t *testing.T, input []byte) {
		var checks []Check
		statuses := []Status{StatusOK, StatusInfo, StatusWarn, StatusFail}
		for i, b := range input {
			checks = append(checks, Check{ID: fmt.Sprintf("check.%d", i), Category: "config", Status: statuses[int(b)%len(statuses)]})
		}
		data, err := json.Marshal(NewReport(checks))
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(data, &wire))
		rows, ok := wire["checks"].([]any)
		require.True(t, ok, "checks must always encode as a JSON array")
		require.Len(t, rows, len(input))
		for i, row := range rows {
			check := row.(map[string]any)
			require.Equal(t, fmt.Sprintf("check.%d", i), check["id"])
			require.Equal(t, string(statuses[int(input[i])%len(statuses)]), check["status"])
			details, ok := check["details"].([]any)
			require.True(t, ok, "details must always encode as a JSON array")
			require.Empty(t, details)
		}
	})
}
