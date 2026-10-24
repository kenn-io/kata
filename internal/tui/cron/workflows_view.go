package cron

import (
	"encoding/json/jsontext"
	"sort"
	"strings"
	"time"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type workflowKey struct{ projectUID, uid string }
type workflowJob struct {
	index                     int
	key, name, profile, input string
	timeout                   time.Duration
}
type workflowView struct {
	index         int
	key           workflowKey
	check         workflowCheck
	laneAvailable bool
	active        bool
	stepLabels    []string
	jobs          []workflowJob
	runs          []int
	search        string
}
type workflowIndex map[workflowKey]*workflowView

func buildWorkflowIndex(d Data, jobs jobIndex, now time.Time) workflowIndex {
	index := workflowIndex{}
	for i, w := range d.Workflows {
		if w.DeletedAt != nil {
			continue
		}
		key := workflowKey{effectiveProjectUID(d, w.ProjectUID), w.UID}
		v := &workflowView{index: i, key: key, laneAvailable: true, check: workflowShape(w.Definition)}
		for ordinal := range v.check.Order {
			v.stepLabels = append(v.stepLabels, workflowStepLabel(v.check, ordinal))
		}
		index[key] = v
	}
	for _, j := range jobs.byKey {
		if j.decodeErr != nil || j.def.Action.Kind != "execute" || j.def.Action.WorkflowUID == "" {
			continue
		}
		v := index[workflowKey{j.key.projectUID, j.def.Action.WorkflowUID}]
		if v == nil {
			continue
		}
		item := workflowJob{index: j.index, key: scopedRowKey(j.key.projectUID, j.key.uid), name: j.entry.Name, profile: j.profile, input: j.input, timeout: time.Duration(j.def.TimeoutSeconds) * time.Second}
		v.jobs = append(v.jobs, item)
		v.active = v.active || j.def.Enabled
	}
	for i, r := range d.Runs {
		if r.WorkflowUID != nil {
			if v := index[workflowKey{r.ProjectKey, records.Value(r.WorkflowUID)}]; v != nil {
				v.runs = append(v.runs, i)
			}
		}
	}
	for _, v := range index {

		sort.Slice(v.jobs, func(i, j int) bool {
			a, b := v.jobs[i], v.jobs[j]
			if strings.EqualFold(a.name, b.name) {
				return a.key < b.key
			}
			return strings.ToLower(a.name) < strings.ToLower(b.name)
		})
		w := d.Workflows[v.index]
		fields := []string{w.Project, w.UID, w.Name, strings.Join(v.check.Unsupported, " "), strings.Join(v.check.JobDependent, " "), strings.Join(v.check.Warnings, " ")}
		if !v.laneAvailable {
			fields = append(fields, "lane unavailable")
		}
		if v.check.DecodeErr != nil {
			fields = append(fields, "does not parse", v.check.DecodeErr.Error())
		}
		if def := v.check.Definition; def != nil {
			fields = append(fields, def.About, def.Input)
			for _, s := range def.Steps {
				fields = append(fields, s.Key, s.Kind)
			}
		}
		if len(v.runs) > 0 {
			r := d.Runs[v.runs[0]]
			label, _ := runLabel(r)
			detail, _ := runNote(r, now)
			fields = append(fields, label, r.Status, detail)
		}
		v.search = strings.ToLower(strings.Join(fields, " "))
	}
	return index
}
func (m *model) workflowRows() []row {
	type rankedView struct {
		view *workflowView
		rank int
	}
	views := make([]rankedView, 0, len(m.wfIndex))
	for _, v := range m.wfIndex {
		rank := 2
		if v.check.DecodeErr != nil || len(v.check.Unsupported) > 0 {
			rank = 0
		} else if !v.laneAvailable {
			rank = 1
		}
		views = append(views, rankedView{view: v, rank: rank})
	}
	sort.Slice(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		wa, wb := m.data.Workflows[a.view.index], m.data.Workflows[b.view.index]
		if wa.Project != wb.Project {
			return wa.Project < wb.Project
		}
		if strings.EqualFold(wa.Name, wb.Name) {
			return wa.UID < wb.UID
		}
		return strings.ToLower(wa.Name) < strings.ToLower(wb.Name)
	})
	rows := make([]row, 0, len(views))
	for _, ranked := range views {
		v := ranked.view
		w := m.data.Workflows[v.index]
		rows = append(rows, row{kind: "workflow", projectUID: v.key.projectUID, uid: v.key.uid, label: w.Name, search: v.search, wf: v.index})
	}
	return rows
}

func (m *model) selectedWorkflow() *workflowView {
	r, ok := m.selected()
	if !ok || r.kind != "workflow" {
		return nil
	}
	return m.wfIndex[workflowKey{r.projectUID, r.uid}]
}

type workflowCheck struct {
	Definition                                 *defs.WorkflowDef
	DecodeErr                                  error
	Unsupported, JobDependent, Warnings, Order []string
}

func workflowShape(raw jsontext.Value) workflowCheck {
	w, err := defs.DecodeWorkflow(raw)
	if err != nil {
		return workflowCheck{DecodeErr: err}
	}
	c := workflowCheck{Definition: &w}
	c.Order, err = defs.TopoOrder(w.Steps)
	if err != nil {
		c.Unsupported = append(c.Unsupported, err.Error())
	}
	if err := defs.ValidateWorkflow(raw); err != nil {
		c.Unsupported = append(c.Unsupported, err.Error())
	}
	return c
}
