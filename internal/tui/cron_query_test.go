package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

// Contract: OR within a key, AND across keys and free text; health/status
// are exact, issue is a UID prefix, remaining fields are substrings.
func TestCronQueryExamples(t *testing.T) {
	fields := map[string]string{"status": "failed", "health": "never ran", "job": "Daily summary 01ABC123", "actor": "Operator", "executor": "worker-a", "teammate": "reader", "issue": "01XYZ789", "action": "execute workflow", "trigger": "cron"}
	for _, tc := range []struct {
		query string
		tab   int
		want  bool
	}{
		{"status:FAILED", cronRuns, true}, {"status:fail", cronRuns, false},
		{"status:running status:failed actor:oper", cronRuns, true},
		{"status:failed actor:other", cronRuns, false}, {"report status:failed", cronRuns, true},
		{"missing status:failed", cronRuns, false}, {"job:abc1", cronRuns, true},
		{"executor:KER teammate:read", cronRuns, true}, {"issue:01x", cronRuns, true},
		{"issue:xyz", cronRuns, false}, {"issue:none", cronRuns, false},
		{"health:never-ran", cronJobs, true}, {"health:never", cronJobs, false}, {"action:EXEC trigger:cron", cronJobs, true},
		{"unknown:value", cronRuns, false}, {"https://daemon.example", cronRuns, true},
		{"actor:", cronRuns, true}, {" status:failed ", cronRuns, true},
		{"  report  ", cronRuns, false}, {"status:failed", cronWorkflows, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			require.Equal(t, tc.want, cronQuery(tc.query, tc.tab).match(fields, "Report actor: https://daemon.example"))
		})
	}
	require.True(t, cronQuery("issue:none", cronRuns).match(map[string]string{}, ""))
	require.False(t, cronQuery("issue:none", cronRuns).match(map[string]string{"issue": "none-prefix"}, ""))
}

func TestCronQueryLiteralMatchesOldSubstring(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		query, text := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
		// Workflows are the public literal-only sibling of both token-aware tabs.
		if got, want := cronQuery(query, cronWorkflows).match(nil, text), cronMatches(text, query); got != want {
			ht.Fatalf("literal query changed: %q in %q", query, text)
		}
	}, hegel.WithTestCases(2000))
}

func TestCronQueryNewKeyCannotAddRows(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.SampledFrom([]string{"failed", "running", "succeeded", "unknown", "cancelled"}))
		actor := hegel.Draw(ht, hegel.Text())
		fields := map[string]string{"status": status, "actor": actor}
		base := cronQuery("status:failed status:running", cronRuns)
		added := cronQuery("status:failed status:running actor:operator", cronRuns)
		if got, want := added.match(fields, ""), base.match(fields, "") && strings.Contains(strings.ToLower(actor), "operator"); got != want {
			ht.Fatal("new key must intersect the actor match")
		}
	}, hegel.WithTestCases(2000))
}

func TestCronQuerySameKeyCannotRemoveRows(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.SampledFrom([]string{"failed", "running", "succeeded", "unknown", "cancelled"}))
		fields := map[string]string{"status": status}
		if cronQuery("status:failed", cronRuns).match(fields, "") && !cronQuery("status:failed status:running", cronRuns).match(fields, "") {
			ht.Fatal("OR expansion removed a row")
		}
		if got, want := cronQuery("status:failed status:running", cronRuns).match(fields, ""), status == "failed" || status == "running"; got != want {
			ht.Fatal("fixed OR query must match exactly either status")
		}
	}, hegel.WithTestCases(2000))
}

func TestCronQueryArbitraryInputTotal(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		fields := map[string]string{"status": "failed", "health": "never ran", "actor": "Operator", "issue": "01ABC"}
		for _, tab := range []int{cronJobs, cronRuns, cronWorkflows} {
			parsed := cronQuery(query, tab)
			if parsed.match(nil, "") && strings.TrimSpace(query) != "" && len(parsed.tokens) == 0 && query != "" {
				ht.Fatalf("nonempty literal matches empty text: %q", query)
			}
			if got, want := cronQuery(query, tab).match(fields, "Report actor:"), cronQuery(strings.ToLower(query), tab).match(fields, "Report actor:"); got != want {
				ht.Fatalf("case-fold changed arbitrary query %q in tab %d", query, tab)
			}
		}
	}, hegel.WithTestCases(2000))
}

func TestCronQueryUsesUntruncatedFields(t *testing.T) {
	defer snapshotInit(t)()
	m := cronFixture(t)
	m.cron.data.jobs[0].Name = strings.Repeat("a", 100) + "tail"
	m = m.cronFilter("health:ok action:workflow tail")
	title, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, title, "filtered")
	m = sendRune(m, '3')
	r := &m.cron.runPage.runs[0]
	r.Teammate = new("reader")
	r.IssueUID = new("01ABC123")
	r.Revision++
	m = m.cronRemember([]generated.CronRun{*r})
	m = m.cronFilter("status:succeeded executor:executor-a teammate:reader issue:01abc")
	title, _, lines = cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, title, "filtered")
	require.Equal(t, m.cron.data.runs[0].UID, lines[0].target.uid, "the matching full-value row is reachable")
	assertGolden(t, "cron-runs-filter-tokens", m.viewBody())
}

func TestCronQueryRepeatedKeyORExact(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.SampledFrom([]string{"failed", "running", "succeeded", "unknown", "cancelled"}))
		first := hegel.Draw(ht, hegel.SampledFrom([]string{"failed", "running", "succeeded", "unknown", "cancelled"}))
		second := hegel.Draw(ht, hegel.SampledFrom([]string{"failed", "running", "succeeded", "unknown", "cancelled"}))
		got := cronQuery(fmt.Sprintf("status:%s status:%s", first, second), cronRuns).match(map[string]string{"status": status}, "")
		if got != (status == first || status == second) {
			ht.Fatalf("OR mismatch: %q vs %q/%q", status, first, second)
		}
	}, hegel.WithTestCases(2000))
}
