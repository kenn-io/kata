package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"sort"
	"strings"
	"time"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func (m *model) indexedJob(r row) *jobView {
	if r.kind != "job" || m.jobs == nil {
		return nil
	}
	return m.jobs.byKey[jobKey{r.projectUID, r.uid}]
}

type jobKey struct{ projectUID, uid string }
type jobView struct {
	key                              jobKey
	entry                            records.Job
	job                              records.Job
	def                              defs.JobDef
	decodeErr                        error
	workflow                         *defs.WorkflowDef
	workflowName                     string
	shape, profile, schedule, search string
	issue                            string // The newest run's issue, which i opens.
	tags                             []string
	description                      string
	input                            string
	otherOptions                     []string
	runIndexes                       []int
	index                            int
}
type decodedJob struct {
	event, raw string
	def        defs.JobDef
	err        error
}
type decodedWorkflow struct {
	event, raw string
	def        defs.WorkflowDef
	err        error
}
type jobIndex struct {
	byKey        map[jobKey]*jobView
	jobMemo      map[jobKey]*decodedJob
	workflowMemo map[jobKey]*decodedWorkflow
}

func jobShape(d defs.JobDef) string {
	if d.Action.Kind == "notify" {
		if d.Issue != nil && d.Issue.Kind == "per-run" {
			return "recurring"
		}
		return "notify"
	}
	if d.Action.WorkflowUID != "" {
		return "workflow"
	}
	return "prompt"
}
func jobSchedule(t defs.Trigger) string {
	text := jobScheduleValue(t)
	if t.Timezone != "" && t.Timezone != "UTC" {
		text += " (" + t.Timezone + ")"
	}
	return text
}
func jobScheduleValue(t defs.Trigger) string {
	switch t.Kind {
	case "cron":
		return t.Cron
	case "interval":
		return "every " + (time.Duration(t.IntervalSeconds) * time.Second).String()
	case "once":
		at, err := time.Parse(time.RFC3339Nano, t.At)
		zone := time.UTC
		if t.Timezone != "" {
			var e error
			zone, e = time.LoadLocation(t.Timezone)
			if e != nil {
				return "once " + t.At
			}
		}
		if err != nil {
			return "once " + t.At
		}
		at = at.In(zone)
		format := "2006-01-02 15:04"
		if at.Second() != 0 || at.Nanosecond() != 0 {
			format += ":05.999999999"
		}
		return "once " + at.Format(format)
	case "manual":
		return "manual"
	}
	return t.Kind
}
func (m *model) rebuildJobIndex() {
	old := m.jobs
	index := &jobIndex{byKey: map[jobKey]*jobView{}, jobMemo: map[jobKey]*decodedJob{}, workflowMemo: map[jobKey]*decodedWorkflow{}}
	workflows := map[jobKey]records.Workflow{}
	for _, w := range m.data.Workflows {
		if w.DeletedAt != nil {
			continue
		}
		key := jobKey{effectiveProjectUID(m.data, w.ProjectUID), w.UID}
		workflows[key] = w
		var cached *decodedWorkflow
		if old != nil {
			cached = old.workflowMemo[key]
		}
		if cached == nil || cached.event != w.DefinitionEventUID || cached.raw != string(w.Definition) {
			d, err := defs.DecodeWorkflow(w.Definition)
			cached = &decodedWorkflow{w.DefinitionEventUID, string(w.Definition), d, err}
		}
		index.workflowMemo[key] = cached
	}
	refs := m.issueRefIndex()
	groups := map[jobKey][]int{}
	for i, r := range m.data.Runs {
		key := jobKey{r.ProjectKey, records.Value(r.JobUID)}
		groups[key] = append(groups[key], i)
	}
	for i, entry := range m.data.Jobs {
		if entry.DeletedAt != nil {
			continue
		}
		key := jobKey{effectiveProjectUID(m.data, entry.ProjectUID), entry.UID}
		var cached *decodedJob
		if old != nil {
			cached = old.jobMemo[key]
		}
		if cached == nil || cached.event != entry.DefinitionEventUID || cached.raw != string(entry.Definition) {
			d, err := defs.DecodeJob(entry.Definition)
			cached = &decodedJob{entry.DefinitionEventUID, string(entry.Definition), d, err}
		}
		index.jobMemo[key] = cached
		v := &jobView{key: key, entry: entry, def: cached.def, decodeErr: cached.err, runIndexes: groups[key], index: i}
		v.job = records.Job{ProjectUID: key.projectUID, Project: entry.Project, UID: entry.UID, Name: entry.Name, DefinitionEventUID: entry.DefinitionEventUID, Definition: entry.Definition, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt, DeletedAt: entry.DeletedAt}
		v.shape = jobShape(v.def)
		v.schedule = jobSchedule(v.def.Trigger)
		if v.shape == "workflow" {
			wk := jobKey{key.projectUID, v.def.Action.WorkflowUID}
			if w, ok := workflows[wk]; ok {
				v.workflowName = w.Name
				if memo := index.workflowMemo[wk]; memo != nil && memo.err == nil {
					v.workflow = &memo.def
				}
			}
		}
		if v.shape != "recurring" {
			v.tags, v.description, _ = defs.JobMetadata(v.def.Options)
		}
		v.profile = defs.OptionString(v.def.Options, "agent")
		v.input = defs.OptionString(v.def.Options, "input")
		if v.shape != "recurring" {
			var fields map[string]jsontext.Value
			_ = json.Unmarshal(v.def.Options, &fields)
			for key := range fields {
				switch key {
				case "agent", "input", "run_parent", "tags", "description":
				default:
					v.otherOptions = append(v.otherOptions, key)
				}
			}
			sort.Strings(v.otherOptions)
		}
		if v.profile == "" {
			v.profile = "default"
		}
		last := ""
		if len(v.runIndexes) > 0 {
			r := m.data.Runs[v.runIndexes[0]]
			last = r.Status
			v.issue = refs.run(m, r)
		}
		if v.issue == "" {
			v.issue = qualifiedIssueRef(m.projectName(key.projectUID), jobIssueUID(v.def))
		}
		project := entry.Project
		if project == "" {
			project = m.projectName(key.projectUID)
		}
		v.search = strings.ToLower(strings.Join([]string{project, entry.UID, entry.Name, v.description, strings.Join(v.tags, " "), v.schedule, v.profile, v.workflowName, last, v.issue}, " "))
		index.byKey[key] = v
	}
	m.jobs = index
}
func (m *model) visibleJobs() []*jobView {
	if m.jobs == nil {
		return nil
	}
	rows := make([]*jobView, 0, len(m.jobs.byKey))
	for _, v := range m.jobs.byKey {
		if strings.Contains(v.search, strings.ToLower(m.query)) {
			rows = append(rows, v)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.entry.CreatedAt.Equal(b.entry.CreatedAt) {
			return a.entry.CreatedAt.Before(b.entry.CreatedAt)
		}
		if a.key.uid != b.key.uid {
			return a.key.uid < b.key.uid
		}
		return a.key.projectUID < b.key.projectUID
	})
	return rows
}

// jobIssueUID follows explicit bindings before an issue-date notification trigger.
func jobIssueUID(d defs.JobDef) string {
	if d.Issue != nil {
		if d.Issue.UID != "" {
			return d.Issue.UID
		}
		if d.Issue.ParentUID != "" {
			return d.Issue.ParentUID
		}
	}
	if d.Action.Kind == "notify" && (d.Trigger.Kind == "issue-scheduled" || d.Trigger.Kind == "issue-deadline") {
		return d.Trigger.IssueUID
	}
	return ""
}
