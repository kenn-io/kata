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

func TestCronQueryArbitraryInputTotal(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		query, text := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
		fields := map[string]string{"status": "failed", "health": "never ran", "actor": "Operator", "issue": "01ABC"}
		for _, tab := range []int{cronJobs, cronRuns, cronWorkflows} {
			parsed := cronQuery(query, tab)
			if got, want := parsed.match(fields, "Report actor:"), cronQuery(strings.ToLower(query), tab).match(fields, "Report actor:"); got != want {
				ht.Fatalf("case-fold changed arbitrary query %q in tab %d", query, tab)
			}
			if tab != cronWorkflows && len(parsed.tokens) != 0 {
				continue // Token semantics have their own contract properties.
			}
			for _, candidate := range []string{text, ""} {
				if got, want := parsed.match(nil, candidate), cronMatches(candidate, query); got != want {
					ht.Fatalf("literal query changed: %q in %q (tab %d)", query, candidate, tab)
				}
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

func TestCronQueryCombinedBlockedFilter(t *testing.T) {
	m := cronSeenFixture(t)
	j := m.cron.data.jobs[0]
	j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
	r := generated.CronRun{UID: "waiting-run", JobUID: new(j.UID), IssueUID: new("waiting-issue"), Status: "running", Actor: "operator", CreatedAt: snapshotFixedNow}
	m.cron.data.jobs = []generated.CronJob{j}
	m.cron.data.runs = []generated.CronRun{r}
	m.cron.runPage.runs = []generated.CronRun{r}
	m.cron.data.lastRuns = map[string]generated.CronRun{j.UID: r}
	m.cron.data.lastErr = map[string]error{}
	m.cron.data.waits = map[string]cronParent{"waiting-issue": {open: true, metadata: map[string]any{"work.attention": "needs-human"}}}
	m.cron.lists[cronJobs].query = "health:blocked status:running"
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0].text, "blocked")
	m.cron.tab = cronRuns
	m.cron.lists[cronRuns].query = "status:running actor:operator"
	_, _, lines = cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0].text, "blocked")
}

// OR within status and AND across actor share one closed-form query oracle.
func TestCronQueryRepeatedStatusAndActorIntersection(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		statuses := []string{"failed", "running", "succeeded", "unknown", "cancelled"}
		status := hegel.Draw(ht, hegel.SampledFrom(statuses))
		first := hegel.Draw(ht, hegel.SampledFrom(statuses))
		second := hegel.Draw(ht, hegel.SampledFrom(statuses))
		actor := hegel.Draw(ht, hegel.Text())
		// Exercise both actor-match directions while preserving arbitrary text.
		if hegel.Draw(ht, hegel.Booleans()) {
			actor = "Operator" + actor
		}
		fields := map[string]string{"status": status, "actor": actor}
		query := fmt.Sprintf("status:%s status:%s", first, second)
		wantStatus := status == first || status == second
		if got := cronQuery(query, cronRuns).match(fields, ""); got != wantStatus {
			ht.Fatalf("OR mismatch: %q vs %q/%q", status, first, second)
		}
		wantActor := strings.Contains(strings.ToLower(actor), "operator")
		if got := cronQuery(query+" actor:operator", cronRuns).match(fields, ""); got != (wantStatus && wantActor) {
			ht.Fatal("new key must intersect the actor match")
		}
		fixed := cronQuery("status:failed status:running", cronRuns)
		if cronQuery("status:failed", cronRuns).match(fields, "") && !fixed.match(fields, "") {
			ht.Fatal("OR expansion removed a row")
		}
		wantFixed := status == "failed" || status == "running"
		if got := fixed.match(fields, ""); got != wantFixed {
			ht.Fatal("fixed OR query must match exactly either status")
		}
		if got := cronQuery("status:failed status:running actor:operator", cronRuns).match(fields, ""); got != (wantFixed && wantActor) {
			ht.Fatal("fixed query must intersect the actor match")
		}
	}, hegel.WithTestCases(2000))
}
