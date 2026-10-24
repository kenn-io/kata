package cron

import (
	"fmt"
	"time"

	"go.kenn.io/kata/internal/tui/cron/records"
)

func (m *model) renderRunDetail() bodyLines {
	d := newDetail(m.width, 14, detailSelection{}, noSelection, nil)
	r, ok := m.detailRun()
	if !ok {
		d.Lines("run removed")
		return d.Body()
	}
	v := m.runView(r)
	label, role := runLabel(r)
	d.Title(v.Job, label, role)
	d.Styled(bodyLine{})
	if m.showProjects() {
		d.KV("project", m.projectName(r.ProjectKey))
	}
	d.KVStyled("outcome", label, role)
	d.KV("trigger", v.Trigger)
	d.KV("occurrence", records.Value(r.OccurrenceKey))
	d.KV("issue", m.runIssueRef(r))
	d.KV("executor", records.Value(r.ExecutorLabel))
	d.KV("actor", r.Actor)
	d.KV("teammate", records.Value(r.Teammate))
	d.KV("run", r.UID)
	d.KV("job", records.Value(r.JobUID))
	d.KV("definition", records.Value(r.DefinitionEventUID))
	d.KV("workflow", records.Value(r.WorkflowUID))
	d.KV("workflow event", records.Value(r.WorkflowDefinitionEventUID))
	d.KV("created", stamp(r.CreatedAt))
	d.KV("updated", stamp(r.UpdatedAt))
	d.KV("revision", fmt.Sprint(r.Revision))
	when := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return stamp(*t)
	}
	d.KV("started", when(r.StartedAt))
	d.KV("ended", when(r.EndedAt))
	if r.StartedAt != nil && r.EndedAt != nil && !r.EndedAt.Before(*r.StartedAt) {
		d.KV("duration", dur(r.EndedAt.Sub(*r.StartedAt)))
	}
	d.Section("EVIDENCE")
	for _, line := range wrapText(r.Summary.Message, max(1, m.width-2)) {
		d.Lines(line)
	}
	if r.Summary.InputTokens > 0 || r.Summary.OutputTokens > 0 {
		d.KV("tokens", fmt.Sprintf("%d input · %d output", r.Summary.InputTokens, r.Summary.OutputTokens))
	}
	for _, j := range m.data.Jobs {
		if j.ProjectUID == r.ProjectKey && j.UID == records.Value(r.JobUID) && records.Value(r.DefinitionEventUID) != "" && j.DefinitionEventUID != records.Value(r.DefinitionEventUID) {
			d.Lines("Job definition changed since this run. Linked definition is current.")
		}
	}
	for _, w := range m.data.Workflows {
		if w.ProjectUID == r.ProjectKey && w.UID == records.Value(r.WorkflowUID) && records.Value(r.WorkflowDefinitionEventUID) != "" && w.DefinitionEventUID != records.Value(r.WorkflowDefinitionEventUID) {
			d.Lines("Workflow definition differs from this observation. Linked definition is current.")
		}
	}
	return d.Body()
}
