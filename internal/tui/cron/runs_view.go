package cron

import (
	"strings"
	"time"

	"go.kenn.io/kata/internal/tui/cron/records"
)

type runViewData struct {
	Run                         records.Run
	Job, Trigger, Search, Issue string
	When                        time.Time
}

// runView uses current catalogs only for names; a run remains recorded evidence.
func runView(r records.Run, d Data) runViewData {
	// Native observations do not record a trigger kind. Occurrence keys are
	// optional identifiers, not evidence of how execution began.
	v := runViewData{Run: r, Trigger: "—"}
	if r.StartedAt != nil {
		v.When = *r.StartedAt
	}
	for _, j := range d.Jobs {
		if j.ProjectUID == r.ProjectKey && j.UID == records.Value(r.JobUID) {
			v.Job = j.Name
			break
		}
	}
	if v.Job == "" {
		for _, w := range d.Workflows {
			if w.ProjectUID == r.ProjectKey && w.UID == records.Value(r.WorkflowUID) {
				v.Job = w.Name
				break
			}
		}
	}
	v.Issue = records.Value(r.IssueUID)
	if v.Job == "" {
		v.Job = v.Issue
	}
	if v.Job == "" {
		v.Job = "run " + r.UID[:min(8, len(r.UID))]
	}
	v.Job = clean(v.Job)
	v.Search = clean(strings.Join([]string{r.UID, records.Value(r.JobUID), records.Value(r.WorkflowUID), r.Status, records.Value(r.OccurrenceKey), r.Summary.Message, records.Value(r.ExecutorLabel), r.Actor}, " "))
	return v
}
func (m *model) runView(r records.Run) runViewData {
	v := runView(r, m.data)
	v.Issue = m.runIssueRef(r)
	return v
}
func runSearch(v runViewData, project string, _ time.Time) string {
	return strings.Join([]string{v.Search, clean(project), clean(v.Issue), v.Job, v.Trigger}, " ")
}
func runExpandable(records.Run) bool { return false }
