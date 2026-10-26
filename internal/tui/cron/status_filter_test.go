package cron

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func statusFilterFixture(t *testing.T) Data {
	t.Helper()
	d := nativeFixture()
	failed, never := d.Jobs[0], d.Jobs[0]
	failed.UID, failed.Name = "job-2", "Nightly sync"
	never.UID, never.Name = "job-3", "Weekly digest"
	if err := json.Unmarshal([]byte(`{"last_run":{"uid":"run-2","status":"failed","created_at":"2026-01-01T08:00:00Z"}}`), &failed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"last_run":null}`), &never); err != nil {
		t.Fatal(err)
	}
	d.Jobs = append(d.Jobs, failed, never)
	cancelled := d.Runs[0]
	cancelled.UID, cancelled.Status = "run-3", "cancelled"
	cancelled.CreatedAt = cancelled.CreatedAt.Add(-time.Hour)
	d.Runs = append(d.Runs, cancelled)
	return d
}

func rowNames(m *model) []string {
	var out []string
	for _, r := range m.rows {
		out = append(out, r.uid)
	}
	return out
}

func pressStatus(m *model) { m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"}) }

func TestCronJobStatusFilterUsesLastResult(t *testing.T) {
	m := newModel(Options{})
	m.Resize(160, 40)
	m.SetData(statusFilterFixture(t))
	want := map[string]string{"": "job-1 job-2 job-3", "running": "", "succeeded": "job-1", "failed": "job-2", "cancelled": "", "unknown": "", "never_ran": "job-3"}
	seen := []string{}
	for range len(want) {
		got := strings.Join(rowNames(m), " ")
		if got != want[m.status] {
			t.Fatalf("status %q rows = %q, want %q", m.status, got, want[m.status])
		}
		seen = append(seen, m.status)
		pressStatus(m)
	}
	if m.status != "" || len(seen) != len(want) {
		t.Fatalf("filter cycle %v did not return to all", seen)
	}
	for range 3 {
		pressStatus(m)
	}
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "status: failed") {
		t.Fatalf("active status filter is not shown:\n%s", text)
	}
}

func TestCronRunStatusFilterSkipsNeverRan(t *testing.T) {
	m := newModel(Options{})
	m.Resize(160, 40)
	m.SetData(statusFilterFixture(t))
	m.SelectTab(1)
	statuses := []string{}
	for {
		pressStatus(m)
		if m.status == "" {
			break
		}
		statuses = append(statuses, m.status)
		if m.status == "cancelled" {
			if got := strings.Join(rowNames(m), " "); got != "run-3" {
				t.Fatalf("cancelled runs = %q", got)
			}
		}
	}
	if got := strings.Join(statuses, " "); got != "running succeeded failed cancelled unknown" {
		t.Fatalf("run filters = %q", got)
	}
}

func TestCronStatusFilterIsPerTab(t *testing.T) {
	m := newModel(Options{})
	m.Resize(160, 40)
	m.SetData(statusFilterFixture(t))
	pressStatus(m)
	pressStatus(m)
	m.SelectTab(1)
	if m.status != "" || len(m.rows) != 2 {
		t.Fatalf("jobs filter leaked into runs: status=%q rows=%v", m.status, rowNames(m))
	}
	m.SelectTab(0)
	if m.status != "succeeded" {
		t.Fatalf("jobs filter was not restored: %q", m.status)
	}
}

// An older daemon omits last_run, so its jobs have no authoritative result and
// only match when loaded history supplies one.
func TestCronJobStatusFilterOlderDaemon(t *testing.T) {
	d := nativeFixture()
	legacy := d.Jobs[0]
	legacy.UID = "job-legacy"
	d.Jobs = append(d.Jobs, legacy)
	if d.Jobs[1].LastRunKnown || d.Jobs[1].LastRun != nil {
		t.Fatal("fixture is not a legacy job")
	}
	m := newModel(Options{})
	m.SetData(d)
	for m.status != "never_ran" {
		pressStatus(m)
	}
	if len(m.rows) != 0 {
		t.Fatalf("legacy job without last_run reported as never ran: %v", rowNames(m))
	}
	for m.status != "succeeded" {
		pressStatus(m)
	}
	if got := strings.Join(rowNames(m), " "); got != "job-1" {
		t.Fatalf("legacy loaded history result = %q", got)
	}
}
