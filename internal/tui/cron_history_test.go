package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func historyDocument(p cronData, ref cronRef, width int) (string, []cronRef) {
	d := cronDoc{width: width}
	cronHistory(&d, p, ref)
	var text strings.Builder
	var links []cronRef
	for _, line := range d.lines {
		text.WriteString(line.text + "\n")
		if line.target.uid != "" && (len(links) == 0 || links[len(links)-1] != line.target) {
			links = append(links, line.target)
		}
	}
	return text.String(), links
}

func TestCronHistoryObservationCounts(t *testing.T) {
	// Every loaded record is an observation, including repeated occurrence keys
	// and the supported unknown status. No duration or scheduled time is inferred.
	hegel.Test(t, func(ht *hegel.T) {
		statuses := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"succeeded", "failed", "cancelled", "running", "unknown"})))
		p := cronData{}
		counts := map[string]int{}
		for i, status := range statuses {
			p.runs = append(p.runs, generated.CronRun{
				UID: fmt.Sprint(i), JobUID: new("job"), Status: status, OccurrenceKey: new("same-occurrence"),
			})
			counts[status]++
		}
		text, _ := historyDocument(p, cronRef{cronJobs, "job"}, 1000)
		if len(statuses) == 0 {
			if strings.Contains(text, "Sample:") || !strings.Contains(text, "no matching loaded observations") {
				ht.Fatalf("wrong empty sample in %q", text)
			}
			return
		}
		if !slices.Contains(strings.Split(text, "\n"), "Sample: "+cronCount(len(statuses), "observation")) {
			ht.Fatalf("sample size missing from %q", text)
		}
		want := fmt.Sprintf("succeeded %d · failed %d · cancelled %d · running %d · unknown %d",
			counts["succeeded"], counts["failed"], counts["cancelled"], counts["running"], counts["unknown"])
		if !slices.Contains(strings.Split(text, "\n"), want) {
			ht.Fatalf("missing exact status counts %q in %q", want, text)
		}
	})
}

func TestCronHistoryRecordedIdentityAndCreationOrder(t *testing.T) {
	for _, kind := range []int{cronJobs, cronWorkflows} {
		t.Run(cronTabNames[kind], func(t *testing.T) {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			p := cronData{runs: []generated.CronRun{
				{UID: "z", Status: "unknown", CreatedAt: at.Add(time.Hour)},
				{UID: "c", Status: "cancelled", CreatedAt: at},
				{UID: "b", Status: "failed", CreatedAt: at},
				{UID: "a", Status: "succeeded", CreatedAt: at.Add(-time.Hour)},
				{UID: "d", Status: "running", CreatedAt: at},
			}}
			for i := range p.runs {
				p.runs[i].JobUID, p.runs[i].WorkflowUID = new("job"), new("old-workflow")
				p.runs[i].OccurrenceKey = new("same-occurrence")
			}
			// Valid evidence can predate recording; strip order uses creation time.
			p.runs[0].StartedAt, p.runs[0].EndedAt = new(at.Add(-2*time.Hour)), new(at.Add(-time.Hour))
			// A terminal record can have no recorded start.
			p.runs[1].EndedAt = new(at)
			// A running record can have no recorded end.
			p.runs[4].StartedAt = new(at)
			// Current association must never rewrite the recorded workflow identity.
			p.jobs = []generated.CronJob{{UID: "job", Definition: generated.CronJobDefinitionResponse{
				Action: generated.ActionResponse{WorkflowUID: new("new-workflow")},
			}}}
			oppositeKind := generated.CronRun{UID: "other-kind-only", Status: "succeeded", CreatedAt: at}
			if kind == cronJobs {
				oppositeKind.WorkflowUID = new("old-workflow")
			} else {
				oppositeKind.JobUID = new("job")
			}
			p.runs = append(p.runs, oppositeKind,
				generated.CronRun{UID: "other", JobUID: new("other"), WorkflowUID: new("other"), Status: "succeeded"})
			ref := cronRef{kind, "job"}
			if kind == cronWorkflows {
				ref.uid = "old-workflow"
			}
			before := slices.Clone(p.runs)
			text, links := historyDocument(p, ref, 200)
			require.Contains(t, text, "Loaded project observations (incomplete)")
			require.Contains(t, text, "Sample: 5 observations")
			require.Contains(t, text, "Loaded run links · newest 100")
			require.NotContains(t, text, "Loaded runs (", "the observation sample already states its size")
			require.Contains(t, text, "succeeded 1 · failed 1 · cancelled 1 · running 1 · unknown 1")
			require.Contains(t, text, "Order: SFCR?")
			require.Contains(t, text, "Oldest left, newest right")
			require.Equal(t, []cronRef{{cronRuns, "z"}, {cronRuns, "c"}, {cronRuns, "b"}, {cronRuns, "a"}, {cronRuns, "d"}}, links)
			require.Equal(t, before, p.runs, "summary must not reorder the loaded links")
			for _, absent := range []string{"rate", "30-day", "Duration", "Lag"} {
				require.NotContains(t, text, absent)
			}
			if kind == cronWorkflows {
				text, _ = historyDocument(p, cronRef{kind, "new-workflow"}, 200)
				require.Contains(t, text, "no matching loaded observations")
			}
		})
	}
}

func TestCronHistoryUnavailableAndEmpty(t *testing.T) {
	p := cronData{runs: []generated.CronRun{{UID: "retained", JobUID: new("job"), Status: "succeeded"}}, runsErr: errors.New("read failed")}
	text, links := historyDocument(p, cronRef{cronJobs, "job"}, 200)
	require.Contains(t, text, "Loaded runs · unavailable")
	require.Contains(t, text, "Loaded project observations (incomplete)")
	require.NotContains(t, text, "Sample:")
	require.NotContains(t, text, "no matching")
	require.Empty(t, links)
	text, _ = historyDocument(cronData{}, cronRef{cronJobs, "job"}, 200)
	require.NotContains(t, text, "Sample:")
	require.Contains(t, text, "no matching loaded observations")
}

func TestCronHistoryNativeJobHasNoInferredObservations(t *testing.T) {
	m := cronFixture(t)
	m.width = 200
	m.cron.data.runs = nil
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&m.cron.data.jobs[0].Definition)
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	text := cronHistoryLinesText(lines)
	require.NotContains(t, text, "Sample:")
	require.Contains(t, text, "no matching loaded observations")
	require.NotContains(t, text, "succeeded 1")
}

func TestCronHistoryNarrowLayout(t *testing.T) {
	m := cronFixture(t)
	for _, width := range []int{12, 24, 40, 80, 120} {
		text, links := historyDocument(*m.cron.data, cronRef{cronJobs, m.cron.data.jobs[0].UID}, width)
		for line := range strings.SplitSeq(text, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width)
		}
		require.Equal(t, []cronRef{{cronRuns, m.cron.data.runs[0].UID}}, links)
	}
}

func TestCronHistoryFullLoadedWindowNarrowLayout(t *testing.T) {
	// The client loads at most 100 project observations. Even a full page is
	// incomplete history; the strip sorts while every link keeps read order.
	statuses := []string{"succeeded", "failed", "cancelled", "running", "unknown"}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := cronData{}
	var wantLinks []cronRef
	for i := 99; i >= 0; i-- {
		uid := fmt.Sprintf("run-%03d", i)
		p.runs = append(p.runs, generated.CronRun{
			UID: uid, JobUID: new("job"), WorkflowUID: new("workflow"),
			Status: statuses[i%len(statuses)], CreatedAt: at,
			OccurrenceKey: new("same-occurrence"),
		})
		wantLinks = append(wantLinks, cronRef{cronRuns, uid})
	}
	before := slices.Clone(p.runs)
	for _, ref := range []cronRef{{cronJobs, "job"}, {cronWorkflows, "workflow"}} {
		for _, width := range []int{12, 40, 80} {
			text, links := historyDocument(p, ref, width)
			// Strip wrapping to check provenance and exact sample text at any width.
			compact := strings.Join(strings.Fields(text), "")
			require.Contains(t, compact, "Loadedprojectobservations(incomplete)")
			require.Contains(t, compact, "Sample:100observations")
			require.Equal(t, wantLinks, links)
			for line := range strings.SplitSeq(text, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), width)
			}
		}
	}
	summary := strings.Split(cronObservationSummary(p.runs), "\n")
	require.Equal(t, "Sample: 100 observations", summary[0])
	require.Equal(t, "succeeded 20 · failed 20 · cancelled 20 · running 20 · unknown 20", summary[1])
	require.Equal(t, "Order: "+strings.Repeat("SFCR?", 20), summary[2])
	require.Equal(t, before, p.runs)
}

func TestCronHistoryRefreshAndSelection(t *testing.T) {
	for _, tab := range []rune{'2', '4'} {
		t.Run(string(tab), func(t *testing.T) {
			m := sendRune(cronFixture(t), tab)
			m.width = 200
			m = sendKey(m, tea.KeyEnter)
			a := &cronReadTestAPI{data: *m.cron.data}
			m.api = a
			m, cmd := updateModel(m, keyRune('r'))
			require.NotNil(t, cmd)
			require.Contains(t, stripANSI(renderCron(m)), "loading…")
			_, _, lines := cronScreen(m)
			require.Contains(t, cronHistoryLinesText(lines), "Sample: 1 observation")
			// Failed refresh keeps rows internally but cannot advertise counts.
			a.data.runsErr = errors.New("offline")
			a.data.runs = nil
			m, _ = updateModel(m, cmd())
			_, _, lines = cronScreen(m)
			text := cronHistoryLinesText(lines)
			require.Contains(t, text, "Loaded runs · unavailable")
			require.NotContains(t, text, "Sample:")
			// Successful retry replaces the sample and keeps the detail open.
			a.data.runsErr = nil
			a.data.runs = []generated.CronRun{{UID: "new", JobUID: new(m.cron.data.jobs[0].UID),
				WorkflowUID: new(m.cron.data.workflows[0].UID), Status: "failed"}}
			m, cmd = updateModel(m, keyRune('r'))
			m, _ = updateModel(m, cmd())
			_, _, lines = cronScreen(m)
			text = cronHistoryLinesText(lines)
			require.Contains(t, text, "failed 1")
			require.Contains(t, text, "succeeded 0")
			require.Len(t, m.cron.stack, 1)
			// A different selected definition sees only its own observations.
			ref := m.cron.stack[0].ref
			ref.uid = "different"
			text, links := historyDocument(*m.cron.data, ref, 200)
			require.Contains(t, text, "no matching loaded observations")
			require.Empty(t, links)
		})
	}
	m := cronFixture(t)
	m.cron.data = nil
	require.Contains(t, stripANSI(renderCron(m)), "loading cron data…")
	require.NotContains(t, stripANSI(renderCron(m)), "Sample: 0")
}

func cronHistoryLinesText(lines []cronLine) string {
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.text + "\n")
	}
	return text.String()
}
