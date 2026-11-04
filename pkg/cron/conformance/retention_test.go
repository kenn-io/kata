package conformance

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestSkipCatchupAppliesDeclaredCapsAfterGrace(t *testing.T) {
	t.Run("count cap", func(t *testing.T) {
		in := testPlan(t)
		in.Now = in.Job.CreatedAt.Add(201 * time.Minute)
		in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
		in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), `"catchup":"all"`, `"catchup":"skip","grace_seconds":86400`))
		d := Reference{}.Declare()
		d.CatchupLimit = 100
		out, err := (Reference{Policy: d}).Plan(t.Context(), in)
		require.NoError(t, err)
		require.Len(t, out.Launch, 100)
		require.Equal(t, in.Now.Add(-99*time.Minute), out.Launch[0])
	})

	t.Run("age window", func(t *testing.T) {
		in := testPlan(t)
		in.Now = in.Job.CreatedAt.Add(48 * time.Hour)
		in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), `"catchup":"all"`, `"catchup":"skip","grace_seconds":172801`))
		d := Reference{}.Declare()
		d.CatchupLimit = 0
		d.CatchupWindowSeconds = 86400
		out, err := (Reference{Policy: d}).Plan(t.Context(), in)
		require.NoError(t, err)
		require.Len(t, out.Launch, 25)
		require.Equal(t, in.Now.Add(-24*time.Hour), out.Launch[0])
	})
}

func TestSuiteCountBoundaryIncludesFuturePendingWithAgeWindow(t *testing.T) {
	d := Reference{}.Declare()
	d.CatchupLimit = 100000
	d.CatchupWindowSeconds = 86400
	suite, err := Suite(t.Context(), d)
	require.NoError(t, err)
	var countBoundary Scenario
	for _, scenario := range suite.Scenarios {
		if scenario.Name == "catchup/policy-count-boundary" {
			countBoundary = scenario
			break
		}
	}
	require.NotEmpty(t, countBoundary.Name)
	var expected PlanOutput
	require.NoError(t, json.Unmarshal(countBoundary.Steps[0].Expect, &expected))
	require.Empty(t, expected.Launch)
	require.Len(t, expected.Checkpoint.Pending, 100000)
}
