package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/termtext"
)

var cronTabNames = [...]string{"kata カタ", "jobs", "runs", "workflows"}

type cronLine struct {
	text   string
	target cronRef
}

func cronTabs(tab int) string {
	names := slices.Clone(cronTabNames[:])
	names[tab] = tabActive.Render("[" + names[tab] + "]")
	return strings.Join(names, " ")
}
func cronSchedule(t generated.TriggerResponse) string {
	value := t.Kind
	switch t.Kind {
	case "cron":
		value = deref(t.Cron)
	case "interval":
		value = "every " + (time.Duration(deref(t.IntervalSeconds)) * time.Second).String()
	case "once":
		value = "once " + deref(t.At)
		if at, err := time.Parse(time.RFC3339Nano, deref(t.At)); err == nil {
			zone := time.UTC
			if deref(t.Timezone) != "" {
				if loc, err := time.LoadLocation(deref(t.Timezone)); err == nil {
					zone = loc
				}
			}
			value = "once " + at.In(zone).Format("2006-01-02 15:04")
		}
	}
	if deref(t.Timezone) != "" && deref(t.Timezone) != "UTC" {
		value += " (" + deref(t.Timezone) + ")"
	}
	return value
}
func cronAction(p cronData, j generated.CronJob) string {
	if j.Definition.Action.Kind == "notify" {
		return "notify"
	}
	if uid := deref(j.Definition.Action.WorkflowUID); uid != "" {
		return "workflow " + cronDefinitionName(p, uid, true)
	}
	return "prompt"
}
func cronTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("01-02 15:04")
}
func cronLast(runs []generated.CronRun, uid string, workflow bool) string {
	for _, r := range runs {
		ref := r.JobUID
		if workflow {
			ref = r.WorkflowUID
		}
		if deref(ref) == uid {
			return r.Status + " " + humanizeRelative(r.CreatedAt)
		}
	}
	return "—"
}
func cronCount(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

func cronWorkflowUsage(p cronData, uid string) string {
	if p.jobsErr != nil {
		return "unknown"
	}
	return cronCount(len(p.workflowJobs(uid)), "job")
}

func cronScreen(m Model) (string, string, []cronLine) {
	if len(m.cron.stack) > 0 {
		return cronDetail(m, m.cron.stack[len(m.cron.stack)-1].ref)
	}
	headers := [][]string{
		nil,
		{"Job", "State", "Trigger", "Action", "Last loaded run"},
		{"Status", "Job", "Occurrence", "Started", "Ended", "Summary"},
		{"Workflow", "Steps", "Used by", "Last loaded run", "Updated"},
	}[m.cron.tab]
	// Fixed columns leave the name and summary the remaining cells.
	widths := [][]int{
		nil,
		{0, 7, 17, 18, 18},
		{10, 0, 12, 11, 11, 0},
		{0, 5, 7, 18, 11},
	}[m.cron.tab]
	fixed := 2 + len(widths) - 1
	flex := 0
	for _, w := range widths {
		fixed += w
		if w == 0 {
			flex++
		}
	}
	widths = slices.Clone(widths)
	for i, w := range widths {
		if w == 0 {
			widths[i] = max((m.width-fixed)/flex, 1)
		}
	}
	format := func(cells []string) string {
		out := make([]string, len(cells))
		for i, c := range cells {
			out[i] = padToWidth(truncate(sanitizeForLine(c), widths[i]), widths[i])
		}
		return strings.Join(out, " ")
	}
	lines := []cronLine{}
	add := func(uid string, cells []string) {
		if cronMatches(sanitizeForLine(strings.Join(cells, " ")), m.cron.lists[m.cron.tab].query) {
			lines = append(lines, cronLine{format(cells), cronRef{m.cron.tab, uid}})
		}
	}
	if p := m.cron.data; p != nil {
		runs := p.runs
		switch m.cron.tab {
		case cronJobs:
			for _, j := range p.jobs {
				add(j.UID, []string{j.Name, cronJobState(j), cronSchedule(j.Definition.Trigger), cronAction(*p, j), cronLast(runs, j.UID, false)})
			}
		case cronRuns:
			for _, r := range runs {
				add(r.UID, []string{r.Status, cronDefinitionName(*p, deref(r.JobUID), false), deref(r.OccurrenceKey), cronTime(r.StartedAt), cronTime(r.EndedAt), deref(r.Summary.Message)})
			}
		case cronWorkflows:
			for _, w := range p.workflows {
				add(w.UID, []string{w.Name, fmt.Sprint(len(w.Definition.Steps)), cronWorkflowUsage(*p, w.UID), cronLast(runs, w.UID, true), cronTime(&w.UpdatedAt)})
			}
		}
	}
	noun := []string{"", "job", "run", "workflow"}[m.cron.tab]
	title := cronCount(len(lines), noun)
	if m.cron.tab == cronRuns {
		title = cronCount(len(lines), "loaded run") + " · newest 100"
	}
	return title, format(headers), lines
}

// Lists and documents share a line cursor and viewport. Links retain their
// target on every wrapped line so ordinary movement reaches all content.
type cronDoc struct {
	width      int
	labelWidth int
	lines      []cronLine
}

func (d *cronDoc) text(value string, target cronRef) {
	for _, line := range termtext.Wrap(sanitizeForDisplay(value), max(d.width, 1)) {
		d.lines = append(d.lines, cronLine{text: line, target: target})
	}
}
func (d *cronDoc) field(label, value string) {
	if value != "" {
		d.text(fmt.Sprintf("%-*s %s", d.labelWidth, label+":", value), cronRef{})
	}
}
func (d *cronDoc) section(label string) {
	d.text("\n"+label, cronRef{})
}
func (d *cronDoc) json(value any) {
	b, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		d.text(err.Error(), cronRef{})
		return
	}
	d.text(string(b), cronRef{})
}

func cronDetail(m Model, ref cronRef) (string, string, []cronLine) {
	d := cronDoc{width: m.width - 2, labelWidth: []int{0, 9, 11, 8}[ref.kind]}
	p := cronData{}
	if m.cron.data != nil {
		p = *m.cron.data
	}
	name := ""
	switch ref.kind {
	case cronJobs:
		if j := p.job(ref.uid); j != nil {
			name = j.Name
			d.field("State", cronJobState(*j))
			d.field("Schedule", cronSchedule(j.Definition.Trigger))
			d.field("Action", cronAction(p, *j))
			if uid := deref(j.Definition.Action.WorkflowUID); uid != "" {
				cronDefinitionLink(&d, p, uid, "", true)
			}
			def := j.Definition
			if deref(def.Action.Prompt) != "" {
				d.section("Prompt")
				d.text(deref(def.Action.Prompt), cronRef{})
				def.Action.Prompt = nil
			}
			if deref(def.Action.Message) != "" {
				d.section("Message")
				d.text(deref(def.Action.Message), cronRef{})
				def.Action.Message = nil
			}
			d.section("Definition")
			d.json(def)
			cronHistory(&d, p, ref)
		}
	case cronRuns:
		for _, r := range p.runs {
			if r.UID != ref.uid {
				continue
			}
			name = r.UID
			d.field("Status", r.Status)
			d.field("Occurrence", deref(r.OccurrenceKey))
			d.field("Started", cronTime(r.StartedAt))
			d.field("Ended", cronTime(r.EndedAt))
			d.field("Recorded", r.CreatedAt.Format(time.RFC3339))
			d.field("Actor", r.Actor)
			d.field("Executor", ownerText(r.ExecutorLabel))
			d.field("Teammate", ownerText(r.Teammate))
			d.field("Issue", deref(r.IssueUID))
			if r.Summary.InputTokens != nil || r.Summary.OutputTokens != nil {
				d.field("Tokens", fmt.Sprintf("%d input, %d output", deref(r.Summary.InputTokens), deref(r.Summary.OutputTokens)))
			}
			d.section("Message")
			d.text(deref(r.Summary.Message), cronRef{})
			d.section("Recorded with")
			cronDefinitionLink(&d, p, deref(r.JobUID), deref(r.DefinitionEventUID), false)
			cronDefinitionLink(&d, p, deref(r.WorkflowUID), deref(r.WorkflowDefinitionEventUID), true)
		}
	case cronWorkflows:
		if w := p.workflow(ref.uid); w != nil {
			name = w.Name
			d.section("About")
			d.text(deref(w.Definition.About), cronRef{})
			d.section("Input")
			d.text(deref(w.Definition.Input), cronRef{})
			d.section(fmt.Sprintf("Steps (%d)", len(w.Definition.Steps)))
			for _, step := range w.Definition.Steps {
				d.section(step.Key)
				d.field("Kind", step.Kind)
				d.field("After", strings.Join(step.After, ", "))
				d.field("Retries", fmt.Sprint(deref(step.Retries)))
				d.text(deref(step.Command)+deref(step.Prompt), cronRef{})
				if len(step.Options) > 0 {
					d.json(step.Options)
				}
			}
			if len(w.Definition.Options) > 0 {
				d.section("Options")
				d.json(w.Definition.Options)
			}
			jobs := p.workflowJobs(ref.uid)
			if p.jobsErr != nil {
				d.section("Used by · unavailable")
			} else {
				d.section(fmt.Sprintf("Used by (%d)", len(jobs)))
			}
			for _, j := range jobs {
				d.text(j.Name, cronRef{cronJobs, j.UID})
			}
			cronHistory(&d, p, ref)
		}
	}
	if name == "" {
		d.text("this "+[]string{"", "job", "run", "workflow"}[ref.kind]+" is no longer in the loaded data", cronRef{})
	}
	return cronTabNames[ref.kind] + " / " + sanitizeForLine(name), "", d.lines
}
func cronDefinitionLink(d *cronDoc, p cronData, uid, revision string, workflow bool) {
	if uid == "" {
		return
	}
	name, event, kind := uid, "", cronJobs
	if workflow {
		kind = cronWorkflows
		if w := p.workflow(uid); w != nil {
			name, event = w.Name, w.DefinitionEventUID
		}
	} else if j := p.job(uid); j != nil {
		name, event = j.Name, j.DefinitionEventUID
	}
	label := []string{"", "Job", "", "Workflow"}[kind] + ": " + name
	readErr := p.jobsErr
	if workflow {
		readErr = p.workflowsErr
	}
	if readErr != nil {
		d.text(label+" · unavailable", cronRef{})
		return
	}
	if event == "" {
		d.text(label+" · no longer defined", cronRef{})
		return
	}
	if revision != "" && revision != event {
		label += " · current definition changed since this run"
	}
	d.text(label, cronRef{kind, uid})
}
func cronHistory(d *cronDoc, p cronData, ref cronRef) {
	if p.runsErr != nil {
		d.section("Loaded runs · unavailable")
		return
	}
	runs := []generated.CronRun{}
	for _, r := range p.runs {
		uid := r.JobUID
		if ref.kind == cronWorkflows {
			uid = r.WorkflowUID
		}
		if deref(uid) == ref.uid {
			runs = append(runs, r)
		}
	}
	d.section(fmt.Sprintf("Loaded runs (%d) · newest 100", len(runs)))
	if len(runs) == 0 {
		d.text("no loaded runs", cronRef{})
	}
	for _, r := range runs {
		d.text(r.Status+" · "+r.CreatedAt.Format(time.RFC3339)+" · "+deref(r.Summary.Message), cronRef{cronRuns, r.UID})
	}
}

func cronHelpRows(m Model) [][]helplayout.HelpItem {
	return modalFirstHelpRows(m.modal, [][]helplayout.HelpItem{cronKeyRows(m)})
}

func cronKeyRows(m Model) []helplayout.HelpItem {
	open, back := "open", "kata"
	if len(m.cron.stack) > 0 {
		open, back = "follow", "back"
	}
	rows := []helplayout.HelpItem{
		{Key: "j/k", Description: "move"},
		{Key: "↵/l", Description: open},
		{Key: "h/esc", Description: back},
		{Key: "r", Description: "refresh"},
		{Key: "1-4", Description: "tabs"},
		{Key: "?", Description: "help"},
		{Key: "q", Description: "quit"},
	}
	if len(m.cron.stack) == 0 {
		rows = slices.Insert(rows, 2, helplayout.HelpItem{Key: "/", Description: "filter"})
	}
	return rows
}

// cronNoticeRows reserves the rows appendNonListDetailExtras adds below the cron screen.
func cronNoticeRows(m Model) int {
	if notices := m.appendNonListDetailExtras(""); notices != "" {
		return strings.Count(notices, "\n") + 1
	}
	return 0
}
func renderCron(m Model) string {
	if m.scope.allProjects || m.scope.projectID == 0 {
		body := []string{renderTitleBar(m.width, m.scope, kataVersion, m.chrome().daemon), cronTabs(m.cron.tab), "", "Press P to pick a project, then 2 for Jobs, 3 for Runs, or 4 for Workflows."}
		help := cronHelpRows(m)
		for range max(m.height-len(body)-helpLines(help, m.width)-cronNoticeRows(m), 1) {
			body = append(body, "")
		}
		return strings.Join(append(body, renderFooterHelpTable(help, m.width)), "\n")
	}
	title, header, lines := cronScreen(m)
	help := cronHelpRows(m)
	errors := []string{}
	if m.cron.data != nil {
		for _, read := range []struct {
			name string
			err  error
		}{
			{"jobs", m.cron.data.jobsErr},
			{"workflows", m.cron.data.workflowsErr},
			{"runs", m.cron.data.runsErr},
		} {
			if read.err != nil {
				errors = append(errors, errorStyle.Render(truncate(sanitizeForLine(read.name+": "+read.err.Error()), m.width)))
			}
		}
	}
	query := m.cron.lists[m.cron.tab].query
	right := ""
	if query != "" || m.cron.filtering {
		right = "/" + query
		if m.cron.filtering {
			right += "▏"
		}
	} else if m.cron.out {
		right = "loading…"
	}
	body := []string{
		renderTitleBar(m.width, m.scope, kataVersion, m.chrome().daemon),
		cronTabs(m.cron.tab),
		statsLineStyle.Render(padLeftRightInside(title, right, titleBarInnerWidth(m.width))),
		"",
	}
	if header != "" {
		body = append(body, tableHeaderStyle.Render("  "+header))
	}
	budget := max(m.height-len(body)-max(len(errors), 1)-helpLines(help, m.width)-cronNoticeRows(m), 1)
	cursor := min(max(*m.cronCursor(), 0), max(len(lines)-1, 0))
	start, end := windowBounds(len(lines), cursor, budget)
	if len(lines) == 0 {
		empty := "no " + cronTabNames[m.cron.tab]
		switch {
		case m.cron.data == nil:
			empty = "loading cron data…"
		case m.cron.data.readError(m.cron.tab) != nil:
			empty = cronTabNames[m.cron.tab] + " unavailable: see error below"
		case query != "":
			empty += " match " + fmt.Sprintf("%q", query)
		}
		body = append(body, empty)
		budget--
	}
	for i := start; i < end; i++ {
		line := lines[i]
		prefix := "  "
		style := normalRowStyle
		if i%2 == 1 {
			style = altRowStyle
		}
		if line.target.uid != "" && len(m.cron.stack) > 0 {
			style = helpKeyStyle
		}
		if i == cursor {
			prefix = "▶ "
			style = cursorRowStyle
		}
		body = append(body, style.Render(padToWidth(prefix+line.text, m.width)))
		budget--
	}
	for range max(budget, 0) {
		body = append(body, "")
	}
	if len(errors) == 0 {
		errors = append(errors, "")
	}
	body = append(body, errors...)
	body = append(body, renderFooterHelpTable(help, m.width))
	return strings.Join(body, "\n")
}
func cronHelp(m Model) string {
	intro := []string{
		"A job is a schedule plus an action.",
		"A workflow is a named list of steps jobs can run.",
		"A run is what an executor recorded; runs remember the definition revision they used.",
		"kata cron --help shows creation examples.",
	}
	for i, s := range intro {
		intro[i] = strings.Join(termtext.Wrap(s, max(m.width, 1)), "\n")
	}
	body := []string{
		renderTitleBar(m.width, m.scope, kataVersion, m.chrome().daemon),
		cronTabs(m.cron.tab),
		"",
		strings.Join(intro, "\n"),
		"",
	}
	body = append(body, renderHelpGroup([]helpSection{{title: "Cron keys", rows: cronKeyRows(m)}}))
	return strings.Join(body, "\n")
}
