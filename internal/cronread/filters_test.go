package cronread

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

// New predicates require an acknowledgement, while legacy job-only reads do not.
func TestCronFilterEchoCompatibility(t *testing.T) {
	require.NoError(t, VerifyFilters([]byte(`{"runs":[]}`), db.CronRunFilters{JobUID: "01ARZ3NDEKTSV4RRFFQ69G5FA0"}))
	for _, raw := range []string{`{"runs":[]}`, `{"filters":null}`, `{"filters":{}}`, `{"filters":{"status":["succeeded"]}}`} {
		require.ErrorContains(t, VerifyFilters([]byte(raw), db.CronRunFilters{Statuses: []string{"failed"}}), "daemon does not support cron run filters")
	}
}

func TestCronFilterEchoKnownFieldsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "failed", "cancelled", "unknown"}))
		at := time.Unix(int64(hegel.Draw(ht, hegel.Integers(0, 2000000000))), 123456789).UTC()
		label := hegel.Draw(ht, hegel.Text())
		// Echo matching is exact for labels; generation here is independent of request validation.
		filters := db.CronRunFilters{Statuses: []string{status}, ExecutorLabel: label, Since: &at}
		encoded, err := json.Marshal(filters)
		require.NoError(ht, err)
		var fields map[string]any
		require.NoError(ht, json.Unmarshal(encoded, &fields))
		fields["future_filter"] = map[string]any{"any": "value"}
		raw, err := json.Marshal(map[string]any{"filters": fields})
		require.NoError(ht, err)
		require.NoError(ht, VerifyFilters(raw, filters))
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestCronFilterEchoMismatchProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		expected := db.CronRunFilters{Statuses: []string{"failed"}, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FA0", JobUID: "01ARZ3NDEKTSV4RRFFQ69G5FA1", ExecutorLabel: "executor-a", Teammate: "runner", Since: new(time.Unix(1000, 1).UTC())}
		actual := expected
		switch hegel.Draw(ht, hegel.Integers(0, 5)) {
		case 0:
			actual.Statuses = []string{"succeeded"}
		case 1:
			actual.IssueUID = ""
		case 2:
			actual.JobUID = ""
		case 3:
			actual.ExecutorLabel = "executor-b"
		case 4:
			actual.Teammate = "other"
		case 5:
			actual.Since = new(time.Unix(1000, 2).UTC())
		}
		raw, err := json.Marshal(map[string]any{"filters": actual})
		require.NoError(ht, err)
		require.Error(ht, VerifyFilters(raw, expected))
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
