package tui

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// The job form is driven by two tables: a kind says which fields it shows and
// how it reads and builds its part of the definition, and a trigger says its
// own fields and how they build a cron.Trigger. A new kind or trigger is a new
// table entry plus its fields.

type cronField int

const (
	cfKind cronField = iota
	cfName
	cfTrigger
	cfEnabled
	cfCron
	cfZone
	cfInterval
	cfAt
	cfIssue
	cfLead
	cfParent
	cfTitle
	cfBody
	cfPrompt
	cfWorkflow
	cfPolicy
	cfScheduled
	cfDeadline
	cfRecipient
	cfMessage
	cfReport
	cfOverlap
	cfCatchup
	cfGrace
	cfTimeout
	cfCheckout
	cfSecrets
	cfOptions
	cfCount
)

var cronFieldLabels = [cfCount]string{
	cfKind: "Kind", cfName: "Name", cfTrigger: "Trigger", cfEnabled: "Enabled",
	cfCron: "Cron", cfZone: "Time zone", cfInterval: "Interval", cfAt: "At", cfIssue: "Issue", cfLead: "Lead",
	cfParent: "Parent issue", cfTitle: "Issue title", cfBody: "Issue body", cfPrompt: "Prompt", cfWorkflow: "Workflow",
	cfPolicy: "Issue policy", cfScheduled: "Scheduled offset", cfDeadline: "Deadline offset",
	cfRecipient: "Recipient", cfMessage: "Message", cfReport: "Issue",
	cfOverlap: "Overlap", cfCatchup: "Catch up", cfGrace: "Grace", cfTimeout: "Timeout",
	cfCheckout: "Checkout key", cfSecrets: "Secrets", cfOptions: "Options",
}

// cronFieldHints say what a field takes, shown after an empty field.
var cronFieldHints = [cfCount]string{ //nolint:gosec // G101: input hints such as "ENV=source", not credentials
	cfCron: "5 fields like 0 9 * * 1-5, or @daily", cfZone: "IANA name, default UTC",
	cfInterval: "seconds or 15m", cfAt: "2026-11-01T09:00:00+01:00", cfIssue: "issue ref",
	cfLead: "seconds or 2h, optional", cfParent: "issue ref", cfScheduled: "seconds, optional", cfDeadline: "seconds, optional",
	cfRecipient: "blank: current owner or author", cfReport: "issue ref, optional", cfGrace: "seconds or 15m",
	cfTimeout: "seconds or 15m, optional", cfCheckout: "portable name, optional", cfSecrets: "ENV=source, ENV2=source2",
	cfOptions: `JSON object, optional`,
}

type cronChoice struct{ value, label string }

var (
	cronYesNo    = []cronChoice{{"yes", "yes"}, {"no", "no (paused)"}}
	cronPolicies = []cronChoice{{"per-run", "new issue per run"}, {"existing", "existing issue"}}
	cronOverlaps = []cronChoice{{"forbid", "forbid overlapping runs"}, {"allow", "allow overlapping runs"}}
	cronCatchups = []cronChoice{{"latest", "latest missed run"}, {"all", "every missed run"}, {"skip", "skip runs older than the grace"}}
)

// cronRecurringID names the native recurring-child kind.
const cronRecurringID = "recurring"

// cronValidationUID stands in for an issue ref that is not resolved yet, so
// the shape of the definition can be validated while the form is open.
const cronValidationUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type cronJobKind struct {
	id, label string
	triggers  []string
	owns      func(cron.JobDefinition) bool
	shown     func(f *cronForm, field cronField) bool
	load      func(f *cronForm, def cron.JobDefinition)
	build     func(f *cronForm, uidOf func(string) string) (cron.JobDefinition, error)
}

type cronJobTrigger struct {
	id, label string
	fields    []cronField
	load      func(f *cronForm, t cron.Trigger)
	build     func(f *cronForm) (cron.Trigger, error)
}

var (
	allTriggers      = []string{"cron", "interval", "once", "manual", "issue-scheduled", "issue-deadline"}
	cronJobKinds     = []cronJobKind{cronRecurringKind, cronPromptKind, cronWorkflowKind, cronNotifyKind}
	cronJobTriggers  = []cronJobTrigger{cronCronTrigger, cronIntervalTrigger, cronOnceTrigger, cronManualTrigger, cronScheduledTrigger, cronDeadlineTrigger}
	cronExecuteTrigs = allTriggers[:5]
)

// cronFieldError names the field a message belongs to.
type cronFieldError struct {
	field cronField
	text  string
}

func (e *cronFieldError) Error() string { return e.text }

func fieldErr(field cronField, format string, args ...any) error {
	return &cronFieldError{field, fmt.Sprintf(format, args...)}
}

func cronIsDate(trigger string) bool {
	return trigger == "issue-scheduled" || trigger == "issue-deadline"
}

var cronRecurringKind = cronJobKind{
	id: cronRecurringID, label: "recurring child issue", triggers: allTriggers[:2],
	owns: cron.IsRecurringIssueJob,
	shown: func(_ *cronForm, field cronField) bool {
		return slices.Contains([]cronField{cfParent, cfTitle, cfBody, cfRecipient, cfMessage, cfCatchup}, field)
	},
	load: func(f *cronForm, def cron.JobDefinition) {
		f.val[cfParent], f.val[cfTitle], f.val[cfBody] = def.Issue.ParentUID, def.Issue.Title, def.Issue.Body
		f.loadNotification(def)
	},
	build: func(f *cronForm, uidOf func(string) string) (cron.JobDefinition, error) {
		def := f.base()
		def.Overlap, def.Action = "allow", cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
		parent := f.openedParent()
		if f.create {
			parent = uidOf(f.val[cfParent])
		}
		def.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: parent, Title: f.val[cfTitle], Body: f.val[cfBody]}
		f.applyNotification(&def)
		return def, f.applyCatchup(&def)
	},
}

var cronPromptKind = executeKind("prompt", "agent prompt", false)
var cronWorkflowKind = executeKind("workflow", "workflow", true)

func executeKind(id, label string, workflow bool) cronJobKind {
	return cronJobKind{
		id: id, label: label, triggers: cronExecuteTrigs,
		owns: func(def cron.JobDefinition) bool {
			return def.Action.Kind == "execute" && (def.Action.WorkflowUID != "") == workflow
		},
		shown: func(f *cronForm, field cronField) bool {
			switch field {
			case cfPrompt:
				return !workflow
			case cfWorkflow:
				return workflow
			case cfReport:
				// On an issue-date trigger the daemon requires the run issue to be
				// the trigger's own, so there is nothing to ask.
				return f.val[cfPolicy] == "existing" && !cronIsDate(f.trigger)
			case cfTitle, cfBody, cfScheduled, cfDeadline:
				return f.val[cfPolicy] == "per-run"
			}
			return slices.Contains([]cronField{cfPolicy, cfOverlap, cfTimeout, cfCatchup, cfCheckout, cfSecrets, cfOptions}, field)
		},
		load: func(f *cronForm, def cron.JobDefinition) {
			f.val[cfPrompt], f.val[cfWorkflow] = def.Action.Prompt, def.Action.WorkflowUID
			f.val[cfOverlap], f.val[cfCheckout], f.val[cfSecrets] = def.Overlap, def.CheckoutKey, cronFormatSecrets(def.SecretRefs)
			if def.TimeoutSeconds > 0 {
				f.val[cfTimeout] = strconv.FormatInt(def.TimeoutSeconds, 10)
			}
			f.loadCommon(def)
			p := def.Issue
			if p == nil {
				return
			}
			f.val[cfPolicy] = p.Kind
			if p.Kind == "existing" {
				if !cronIsDate(f.trigger) {
					f.val[cfReport] = p.UID
				}
				return
			}
			f.val[cfTitle], f.val[cfBody] = p.Title, p.Body
			for _, offset := range []struct {
				value *int64
				into  cronField
			}{{p.ScheduledOffsetSeconds, cfScheduled}, {p.DeadlineOffsetSeconds, cfDeadline}} {
				if offset.value != nil {
					f.val[offset.into] = strconv.FormatInt(*offset.value, 10)
				}
			}
		},
		build: func(f *cronForm, uidOf func(string) string) (cron.JobDefinition, error) {
			def := f.base()
			def.Overlap, def.Action = f.val[cfOverlap], cron.Action{Kind: "execute"}
			if workflow {
				def.Action.WorkflowUID = f.val[cfWorkflow]
			} else {
				def.Action.Prompt = f.val[cfPrompt]
			}
			def.CheckoutKey = strings.TrimSpace(f.val[cfCheckout])
			secrets, err := cronParseSecrets(f.val[cfSecrets])
			if err != nil {
				return def, err
			}
			def.SecretRefs = secrets
			if err := f.applyOptions(&def); err != nil {
				return def, err
			}
			if def.TimeoutSeconds, err = cronSeconds(cfTimeout, f.val[cfTimeout]); err != nil {
				return def, err
			}
			if def.Issue, err = f.executePolicy(uidOf); err != nil {
				return def, err
			}
			return def, f.applyCatchup(&def)
		},
	}
}

var cronNotifyKind = cronJobKind{
	id: "notify", label: "notify", triggers: allTriggers,
	owns: func(def cron.JobDefinition) bool { return def.Action.Kind == "notify" },
	shown: func(f *cronForm, field cronField) bool {
		switch field {
		case cfIssue:
			return !cronIsDate(f.trigger)
		case cfReport:
			return cronIsDate(f.trigger)
		}
		return slices.Contains([]cronField{cfRecipient, cfMessage, cfOptions, cfCatchup}, field)
	},
	load: func(f *cronForm, def cron.JobDefinition) {
		f.loadNotification(def)
		switch {
		case def.Issue != nil && cronIsDate(def.Trigger.Kind):
			f.val[cfReport] = def.Issue.UID
		case def.Issue != nil:
			f.val[cfIssue] = def.Issue.UID
		}
	},
	build: func(f *cronForm, uidOf func(string) string) (cron.JobDefinition, error) {
		def := f.base()
		def.Overlap, def.Action = cmp.Or(def.Overlap, "allow"), cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
		f.applyNotification(&def)
		switch {
		case !cronIsDate(f.trigger) && strings.TrimSpace(f.val[cfIssue]) != "":
			def.Issue = &cron.IssuePolicy{Kind: "existing", UID: uidOf(f.val[cfIssue])}
		case cronIsDate(f.trigger) && strings.TrimSpace(f.val[cfReport]) != "":
			def.Issue = &cron.IssuePolicy{Kind: "existing", UID: uidOf(f.val[cfReport])}
		}
		if err := f.applyOptions(&def); err != nil {
			return def, err
		}
		return def, f.applyCatchup(&def)
	},
}

var cronCronTrigger = cronJobTrigger{
	id: "cron", label: "cron expression", fields: []cronField{cfCron, cfZone},
	load: func(f *cronForm, t cron.Trigger) { f.val[cfCron], f.val[cfZone] = t.Cron, t.Timezone },
	build: func(f *cronForm) (cron.Trigger, error) {
		return cron.Trigger{Kind: "cron", Cron: strings.TrimSpace(f.val[cfCron]), Timezone: strings.TrimSpace(f.val[cfZone])}, nil
	},
}

var cronIntervalTrigger = cronJobTrigger{
	id: "interval", label: "interval", fields: []cronField{cfInterval},
	load: func(f *cronForm, t cron.Trigger) { f.val[cfInterval] = strconv.FormatInt(t.IntervalSeconds, 10) },
	build: func(f *cronForm) (cron.Trigger, error) {
		seconds, err := cronSeconds(cfInterval, f.val[cfInterval])
		return cron.Trigger{Kind: "interval", IntervalSeconds: seconds}, err
	},
}

var cronOnceTrigger = cronJobTrigger{
	id: "once", label: "once", fields: []cronField{cfAt},
	load: func(f *cronForm, t cron.Trigger) { f.val[cfAt] = t.At },
	build: func(f *cronForm) (cron.Trigger, error) {
		return cron.Trigger{Kind: "once", At: strings.TrimSpace(f.val[cfAt])}, nil
	},
}

var cronManualTrigger = cronJobTrigger{
	id: "manual", label: "manual",
	load:  func(*cronForm, cron.Trigger) {},
	build: func(*cronForm) (cron.Trigger, error) { return cron.Trigger{Kind: "manual"}, nil },
}

var cronScheduledTrigger = cronJobTrigger{
	id: "issue-scheduled", label: "issue's scheduled date", fields: []cronField{cfIssue},
	load:  func(f *cronForm, t cron.Trigger) { f.val[cfIssue] = t.IssueUID },
	build: func(*cronForm) (cron.Trigger, error) { return cron.Trigger{Kind: "issue-scheduled"}, nil },
}

var cronDeadlineTrigger = cronJobTrigger{
	id: "issue-deadline", label: "issue's deadline", fields: []cronField{cfIssue, cfLead},
	load: func(f *cronForm, t cron.Trigger) {
		f.val[cfIssue] = t.IssueUID
		if t.LeadSeconds > 0 {
			f.val[cfLead] = strconv.FormatInt(t.LeadSeconds, 10)
		}
	},
	build: func(f *cronForm) (cron.Trigger, error) {
		lead, err := cronSeconds(cfLead, f.val[cfLead])
		return cron.Trigger{Kind: "issue-deadline", LeadSeconds: lead}, err
	},
}

// cronSeconds reads an optional span as whole seconds: a number or a Go
// duration. Blank is zero; what the number may be is cron's to say.
func cronSeconds(field cronField, text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		d, derr := time.ParseDuration(text)
		if derr != nil || d%time.Second != 0 {
			return 0, fieldErr(field, "enter whole seconds, or a duration like 15m or 2h")
		}
		n = int64(d / time.Second)
	}
	return n, nil
}

func cronParseSecrets(text string) (map[string]string, error) {
	refs := map[string]string{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		name, source, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, source = strings.TrimSpace(name), strings.TrimSpace(source)
		if !ok || name == "" || source == "" {
			return nil, fieldErr(cfSecrets, "use NAME=source pairs separated by commas")
		}
		if _, dup := refs[name]; dup {
			return nil, fieldErr(cfSecrets, "%s appears twice", name)
		}
		refs[name] = source
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return refs, nil
}

func cronFormatSecrets(refs map[string]string) string {
	pairs := make([]string, 0, len(refs))
	for name, source := range refs {
		pairs = append(pairs, name+"="+source)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ", ")
}

// cronForm is the create and edit draft. Text fields hold what was typed; the
// definition is built from them only when validating or saving.
type cronForm struct {
	uid        string // minted when the form opens, so every send of a create is one job
	create     bool
	expected   string // definition_event_uid the edit replaces
	opened     cron.JobDefinition
	openedName string
	openedKind string
	kind       string
	trigger    string
	val        [cfCount]string
	focus      cronField
	ed         textinput.Model
	edInit     string
	snapshot   [cfCount]string
	snapKind   string
	snapTrig   string
	err        *cronFieldError
	general    string
	saving     bool
	conflict   *CronJobRecord
	mine       *CronJobWrite  // what the draft last tried to write, for a conflict apply
	merged     *CronJobRecord // the job a merged form derives from: the base of its next merge
	workflows  []cronChoice
}

func (f *cronForm) kindDef() cronJobKind {
	for _, k := range cronJobKinds {
		if k.id == f.kind {
			return k
		}
	}
	return cronJobKinds[0]
}

func (f *cronForm) triggerDef() cronJobTrigger {
	for _, t := range cronJobTriggers {
		if t.id == f.trigger {
			return t
		}
	}
	return cronJobTriggers[0]
}

// newCronForm is a form for a job that does not exist yet.
func newCronForm(workflows []cronChoice) (*cronForm, error) {
	minted, err := uid.New()
	if err != nil {
		return nil, err
	}
	f := &cronForm{uid: minted, create: true, kind: cronRecurringID, trigger: "cron", workflows: workflows}
	f.defaults()
	f.val[cfEnabled] = "yes"
	f.finish()
	return f, nil
}

// cronFormFor is a form on an existing job. It reads the portable definition
// that was returned by the daemon, so nothing is lost on a round trip.
func cronFormFor(rec CronJobRecord, workflows []cronChoice) (*cronForm, error) {
	f := &cronForm{uid: rec.UID, expected: rec.DefinitionEventUID, opened: rec.Definition, openedName: rec.Name, workflows: workflows}
	if err := f.load(rec); err != nil {
		return nil, err
	}
	f.finish()
	return f, nil
}

func (f *cronForm) load(rec CronJobRecord) error {
	def := rec.Definition
	f.val = [cfCount]string{}
	f.defaults()
	f.val[cfEnabled] = cronYesNoValue(def.Enabled)
	f.val[cfName] = rec.Name
	for _, k := range cronJobKinds {
		if k.owns(def) {
			f.kind, f.openedKind = k.id, k.id
			break
		}
	}
	if f.kind == "" || !slices.Contains(f.kindDef().triggers, def.Trigger.Kind) {
		return fmt.Errorf("the form cannot edit a %s job with a %s trigger", def.Action.Kind, def.Trigger.Kind)
	}
	f.trigger = def.Trigger.Kind
	f.triggerDef().load(f, def.Trigger)
	f.kindDef().load(f, def)
	if len(def.Options) > 0 {
		f.val[cfOptions] = string(def.Options)
		if compact := slices.Clone(def.Options); compact.Compact() == nil {
			f.val[cfOptions] = string(compact)
		}
	}
	return nil
}

func cronYesNoValue(enabled bool) string {
	if enabled {
		return "yes"
	}
	return "no"
}

// defaults are what a field holds until a job or the user says otherwise.
func (f *cronForm) defaults() {
	f.val[cfCron], f.val[cfZone], f.val[cfInterval] = "0 9 * * *", "UTC", "3600"
	f.val[cfPolicy], f.val[cfOverlap], f.val[cfCatchup] = "per-run", "forbid", "latest"
}

func (f *cronForm) loadCommon(def cron.JobDefinition) {
	f.val[cfOverlap] = cmp.Or(def.Overlap, "forbid")
	f.val[cfCatchup] = cmp.Or(def.Catchup, "latest")
	if def.GraceSeconds > 0 {
		f.val[cfGrace] = strconv.FormatInt(def.GraceSeconds, 10)
	}
}

func (f *cronForm) loadNotification(def cron.JobDefinition) {
	if def.Action.Recipient != "current-owner-or-author" {
		f.val[cfRecipient] = def.Action.Recipient
	}
	f.val[cfMessage] = def.Action.Message
	f.loadCommon(def)
}

// finish records the pristine draft, focuses the first field and validates.
func (f *cronForm) finish() {
	f.snapshot, f.snapKind, f.snapTrig = f.val, f.kind, f.trigger
	f.focusField(cfName)
	f.validate()
}

func (f *cronForm) dirty() bool {
	f.syncEditor()
	return f.val != f.snapshot || f.kind != f.snapKind || f.trigger != f.snapTrig
}

// base starts a definition. An edit of the same kind of job keeps what the
// form has no field for; a new or re-kinded job starts clean.
func (f *cronForm) base() cron.JobDefinition {
	def := cron.JobDefinition{Version: 1, Kind: "job"}
	if !f.create && f.kind == f.openedKind {
		def = f.opened
	}
	def.Enabled, def.Issue = f.val[cfEnabled] == "yes", nil
	return def
}

func (f *cronForm) openedParent() string {
	if f.opened.Issue != nil {
		return f.opened.Issue.ParentUID
	}
	return ""
}

func (f *cronForm) applyNotification(def *cron.JobDefinition) {
	if r := strings.TrimSpace(f.val[cfRecipient]); r != "" {
		def.Action.Recipient = r
	}
	def.Action.Message = f.val[cfMessage]
}

func (f *cronForm) applyOptions(def *cron.JobDefinition) error {
	text := strings.TrimSpace(f.val[cfOptions])
	def.Options = nil
	if text == "" {
		return nil
	}
	if !jsontext.Value(text).IsValid() {
		return fieldErr(cfOptions, "is not valid JSON")
	}
	def.Options = jsontext.Value(text)
	return nil
}

// catchupShown says the trigger can fall due while nothing runs it.
func (f *cronForm) catchupShown() bool {
	return slices.Contains([]string{"cron", "interval", "once"}, f.trigger)
}

func (f *cronForm) applyCatchup(def *cron.JobDefinition) error {
	def.Catchup = cmp.Or(f.val[cfCatchup], "latest")
	if !f.catchupShown() || def.Catchup != "skip" {
		return nil
	}
	grace, err := cronSeconds(cfGrace, f.val[cfGrace])
	if err != nil {
		return err
	}
	def.GraceSeconds = grace
	return nil
}

func (f *cronForm) executePolicy(uidOf func(string) string) (*cron.IssuePolicy, error) {
	if f.val[cfPolicy] == "existing" {
		run := cfReport
		if cronIsDate(f.trigger) {
			run = cfIssue
		}
		return &cron.IssuePolicy{Kind: "existing", UID: uidOf(f.val[run])}, nil
	}
	policy := cron.IssuePolicy{Kind: "per-run", Title: f.val[cfTitle], Body: f.val[cfBody]}
	for _, offset := range []struct {
		field cronField
		into  **int64
	}{{cfScheduled, &policy.ScheduledOffsetSeconds}, {cfDeadline, &policy.DeadlineOffsetSeconds}} {
		if strings.TrimSpace(f.val[offset.field]) == "" {
			continue
		}
		n, err := cronSeconds(offset.field, f.val[offset.field])
		if err != nil {
			return nil, err
		}
		*offset.into = &n
	}
	return &policy, nil
}

// shown says a field applies to the form as it stands.
func (f *cronForm) shown(field cronField) bool {
	switch field {
	case cfKind, cfName, cfTrigger, cfEnabled:
		return true
	case cfGrace:
		return f.catchupShown() && f.val[cfCatchup] == "skip" && f.kindDef().shown(f, cfCatchup)
	case cfCatchup:
		return f.catchupShown() && f.kindDef().shown(f, cfCatchup)
	}
	if slices.Contains(f.triggerDef().fields, field) {
		return true
	}
	if slices.Contains([]cronField{cfCron, cfZone, cfInterval, cfAt, cfLead}, field) {
		return false
	}
	return f.kindDef().shown(f, field)
}

// locked says an existing job cannot change the field: the daemon keeps a
// native job's parent and action.
func (f *cronForm) locked(field cronField) bool {
	return !f.create && f.openedKind == cronRecurringID && field == cfParent
}

func (f *cronForm) isChoice(field cronField) bool {
	switch field {
	case cfKind, cfTrigger, cfEnabled, cfPolicy, cfWorkflow, cfOverlap, cfCatchup:
		return true
	}
	return false
}

// kinds are the kinds the form offers: all for a new job, and for an existing
// one only those of its own class, since a native job stays native.
func (f *cronForm) kinds() []cronJobKind {
	if f.create {
		return cronJobKinds
	}
	native := f.openedKind == cronRecurringID
	var kinds []cronJobKind
	for _, k := range cronJobKinds {
		if (k.id == cronRecurringID) == native {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func (f *cronForm) choices(field cronField) []cronChoice {
	var out []cronChoice
	switch field {
	case cfKind:
		for _, k := range f.kinds() {
			out = append(out, cronChoice{k.id, k.label})
		}
	case cfTrigger:
		for _, id := range f.kindDef().triggers {
			for _, t := range cronJobTriggers {
				if t.id == id {
					out = append(out, cronChoice{t.id, t.label})
				}
			}
		}
	case cfEnabled:
		out = cronYesNo
	case cfPolicy:
		out = cronPolicies
	case cfOverlap:
		out = cronOverlaps
	case cfCatchup:
		out = cronCatchups
	case cfWorkflow:
		out = slices.Clone(f.workflows)
		if v := f.val[cfWorkflow]; v != "" && !slices.ContainsFunc(out, func(c cronChoice) bool { return c.value == v }) {
			out = append(out, cronChoice{v, v + " (not a live workflow)"})
		}
	}
	return out
}

func (f *cronForm) current(field cronField) string {
	switch field {
	case cfKind:
		return f.kind
	case cfTrigger:
		return f.trigger
	}
	return f.val[field]
}

func (f *cronForm) display(field cronField) string {
	for _, c := range f.choices(field) {
		if c.value == f.current(field) {
			return c.label
		}
	}
	return f.current(field)
}

// step moves the focused choice field by one choice, wrapping.
func (f *cronForm) step(field cronField, delta int) {
	options := f.choices(field)
	if len(options) == 0 {
		return
	}
	at := slices.IndexFunc(options, func(c cronChoice) bool { return c.value == f.current(field) })
	next := options[(max(at, 0)+delta+len(options))%len(options)].value
	switch field {
	case cfKind:
		f.kind = next
		if !slices.Contains(f.kindDef().triggers, f.trigger) {
			f.trigger = f.kindDef().triggers[0]
		}
	case cfTrigger:
		f.trigger = next
	default:
		f.val[field] = next
	}
	f.validate()
}

// order lists the shown fields in tab order.
func (f *cronForm) order() []cronField {
	out := make([]cronField, 0, cfCount)
	for field := range cronField(cfCount) {
		if f.shown(field) {
			out = append(out, field)
		}
	}
	return out
}

func (f *cronForm) move(delta int) {
	order := f.order()
	if len(order) == 0 {
		return
	}
	i := slices.Index(order, f.focus)
	for range len(order) {
		i = (max(i, 0) + delta + len(order)) % len(order)
		if !f.locked(order[i]) {
			break
		}
	}
	f.focusField(order[i])
}

// syncEditor copies the focused editor back unless it is untouched: a value
// holding line breaks shows flattened in the editor and must not be rewritten.
func (f *cronForm) syncEditor() {
	if f.isChoice(f.focus) || f.locked(f.focus) {
		return
	}
	if v := f.ed.Value(); v != f.edInit {
		f.val[f.focus] = v
		f.edInit = v
	}
}

func (f *cronForm) focusField(field cronField) {
	f.syncEditor()
	f.focus = field
	f.ed = textinput.New()
	f.ed.Prompt = ""
	f.ed.SetValue(f.val[field])
	f.ed.CursorEnd()
	f.ed.Focus()
	f.edInit = f.ed.Value()
}

// validate builds the definition with stand-in issue UIDs and records the
// first problem beside its field.
func (f *cronForm) validate() {
	f.syncEditor()
	_, _, err := f.build(func(ref string) string {
		if strings.TrimSpace(ref) == "" {
			return ""
		}
		return cronValidationUID
	})
	f.err, f.general = nil, ""
	if fe, ok := err.(*cronFieldError); ok {
		f.err = fe
	} else if err != nil {
		f.err = f.attribute(err)
		if f.err == nil {
			f.general = err.Error()
		}
	}
}

// cronErrorWords attributes a message of internal/cron to the field it is
// about; one that matches nothing shows under the form.
var cronErrorWords = []struct {
	word  string
	field cronField
}{
	{"timezone", cfZone}, {"cron expression", cfCron}, {"interval", cfInterval}, {"once", cfAt}, {"lead", cfLead},
	{"issue date", cfIssue}, {"checkout", cfCheckout}, {"secret", cfSecrets}, {"options", cfOptions},
	{"recipient", cfRecipient}, {"message", cfMessage}, {"title", cfTitle}, {"parent", cfParent},
	{"grace", cfGrace}, {"timeout", cfTimeout}, {"offset", cfScheduled}, {"workflow", cfWorkflow},
	{"prompt", cfPrompt}, {"per-run", cfTitle}, {"issue target", cfIssue}, {"existing issue", cfReport}, {"existing issue", cfIssue},
}

// attribute places a message on the shown field its words name. When one
// message names several, the one still blank is blamed.
func (f *cronForm) attribute(err error) *cronFieldError {
	text := strings.TrimPrefix(err.Error(), cron.ErrInvalid.Error()+": ")
	var named []cronField
	for _, w := range cronErrorWords {
		if strings.Contains(text, w.word) && f.shown(w.field) && !slices.Contains(named, w.field) {
			named = append(named, w.field)
		}
	}
	for _, field := range named {
		if strings.TrimSpace(f.current(field)) == "" {
			return &cronFieldError{field, text}
		}
	}
	if len(named) > 0 {
		return &cronFieldError{named[0], text}
	}
	return nil
}

// build reads the form into the job write it would send, or names the
// field that is wrong. uidOf turns an issue ref into its UID.
func (f *cronForm) build(uidOf func(string) string) (string, cron.JobDefinition, error) {
	f.syncEditor()
	name := strings.TrimSpace(f.val[cfName])
	trigger, err := f.triggerDef().build(f)
	if err != nil {
		return "", cron.JobDefinition{}, err
	}
	if !f.create && trigger.Kind != "cron" && trigger.Kind == f.opened.Trigger.Kind && trigger.Timezone == "" {
		trigger.Timezone = f.opened.Trigger.Timezone
	}
	if cronIsDate(trigger.Kind) {
		trigger.IssueUID = uidOf(f.val[cfIssue])
	}
	def, err := f.kindDef().build(f, uidOf)
	if err != nil {
		return "", cron.JobDefinition{}, err
	}
	def.Trigger = trigger
	if err := def.Validate(); err != nil {
		return "", def, err
	}
	return name, def, nil
}

// issueRefs are the refs the draft names that the daemon must resolve to
// UIDs before the write, each with the field to blame when one fails.
func (f *cronForm) issueRefs() []cronFieldRef {
	var refs []cronFieldRef
	for _, field := range []cronField{cfParent, cfIssue, cfReport} {
		ref := strings.TrimSpace(f.val[field])
		if ref == "" || !f.shown(field) || f.locked(field) || cronUIDShaped(ref) {
			continue
		}
		refs = append(refs, cronFieldRef{field, strings.TrimPrefix(ref, "#")})
	}
	return refs
}

type cronFieldRef struct {
	field cronField
	ref   string
}

func cronUIDShaped(ref string) bool {
	return len(ref) == 26 && uid.Valid(strings.ToUpper(ref))
}

// uidResolver maps typed refs to UIDs: UID-shaped text is its own UID.
func cronUIDResolver(resolved map[string]string) func(string) string {
	return func(ref string) string {
		ref = strings.TrimSpace(ref)
		if cronUIDShaped(ref) {
			return strings.ToUpper(ref)
		}
		return resolved[strings.TrimPrefix(ref, "#")]
	}
}

// cronProperties splits a job into the pieces a merge compares: its name and
// each top-level definition member, as the JSON the daemon stores.
func cronProperties(name string, def cron.JobDefinition) map[string]jsontext.Value {
	props := map[string]jsontext.Value{}
	raw, err := json.Marshal(def)
	if err == nil {
		err = json.Unmarshal(raw, &props)
	}
	if err != nil {
		return map[string]jsontext.Value{}
	}
	props["name"], _ = json.Marshal(name)
	return props
}

func cronCanonical(v jsontext.Value) string {
	c := slices.Clone(v)
	if c.Canonicalize() != nil {
		return string(v)
	}
	return string(c)
}

// cronNormalized is the name and definition a form builds, untouched, from
// rec, so values the form adds or drops never read as someone's edit.
func cronNormalized(rec CronJobRecord, workflows []cronChoice) (string, cron.JobDefinition) {
	if f, err := cronFormFor(rec, workflows); err == nil {
		if name, def, err := f.build(cronUIDResolver(nil)); err == nil {
			return name, def
		}
	}
	return rec.Name, rec.Definition
}

// rebase is a three-way merge of the job's name and definition: base is what
// the form opened with, mine is what the draft last tried to write, theirs is
// the job now. A property the draft changed comes from the draft whole;
// every other property comes from the current job, so another writer's edits
// are not undone. Properties both sides changed are named in the notice.
func (f *cronForm) rebase(current CronJobRecord) (*cronForm, error) {
	if f.mine == nil {
		return nil, errors.New("there is no draft to apply")
	}
	baseName, baseDef := f.openedName, f.opened
	if f.merged != nil {
		baseName, baseDef = f.merged.Name, f.merged.Definition
	}
	baseName, baseDef = cronNormalized(CronJobRecord{UID: f.uid, Name: baseName, Definition: baseDef}, f.workflows)
	theirsName, theirsDef := cronNormalized(current, f.workflows)
	base, mine, theirs := cronProperties(baseName, baseDef), cronProperties(f.mine.Name, f.mine.Definition), cronProperties(theirsName, theirsDef)
	merged, both := map[string]jsontext.Value{}, []string{}
	keys := map[string]bool{}
	for _, props := range []map[string]jsontext.Value{base, mine, theirs} {
		for key := range props {
			keys[key] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		pick := theirs[key]
		if cronCanonical(mine[key]) != cronCanonical(base[key]) {
			pick = mine[key]
			if t := cronCanonical(theirs[key]); t != cronCanonical(base[key]) && t != cronCanonical(mine[key]) {
				both = append(both, key)
			}
		}
		if pick != nil {
			merged[key] = pick
		}
	}
	var name string
	if err := json.Unmarshal(merged["name"], &name); err != nil {
		return nil, err
	}
	delete(merged, "name")
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	rec := CronJobRecord{UID: current.UID, Name: name, DefinitionEventUID: current.DefinitionEventUID}
	if err := json.Unmarshal(raw, &rec.Definition); err != nil {
		return nil, err
	}
	next, err := cronFormFor(rec, f.workflows)
	if err != nil {
		return nil, err
	}
	next.merged = &current
	// The draft is unsaved against the job as it is now, whatever the merge kept.
	if persisted, err := cronFormFor(current, f.workflows); err == nil {
		next.snapshot, next.snapKind, next.snapTrig = persisted.snapshot, persisted.snapKind, persisted.snapTrig
	}
	next.focusField(f.focus)
	next.validate()
	// A merged property the form cannot represent builds back as something
	// else; say so rather than claim it was kept.
	var lost []string
	if builtName, built, err := next.build(cronUIDResolver(nil)); err == nil {
		kept, want := cronProperties(builtName, built), cronProperties(name, rec.Definition)
		for key := range want {
			if cronCanonical(kept[key]) != cronCanonical(want[key]) {
				lost = append(lost, key)
			}
		}
		slices.Sort(lost)
	}
	both = slices.DeleteFunc(both, func(key string) bool { return slices.Contains(lost, key) })
	if next.general == "" {
		next.general = "Applied to the current job; review it, then save."
	}
	if len(both) > 0 {
		next.general += " Both changed: " + strings.Join(both, ", ") + ". Your version is kept."
	}
	if len(lost) > 0 {
		next.general += " Could not keep: " + strings.Join(lost, ", ") + "."
	}
	return next, nil
}

// paste inserts pasted text into the focused text field, as typing would.
func (f *cronForm) paste(msg tea.PasteMsg) {
	if f.saving || f.conflict != nil || f.isChoice(f.focus) || f.locked(f.focus) {
		return
	}
	f.ed, _ = f.ed.Update(msg)
	f.validate()
	f.general = ""
}
