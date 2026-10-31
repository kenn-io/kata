package tui

import (
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
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
	cfRecipient: "Recipient", cfMessage: "Message", cfReport: "Notify about issue",
	cfOverlap: "Overlap", cfCatchup: "Catch up", cfGrace: "Grace", cfTimeout: "Timeout",
	cfCheckout: "Checkout key", cfSecrets: "Secrets", cfOptions: "Options",
}

// cronFieldHints say what a field takes, shown after an empty field.
var cronFieldHints = [cfCount]string{
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

// cronMinNativeInterval is the shortest interval the daemon delivers a native job at.
const cronMinNativeInterval = 60

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

func cronIsDate(trigger string) bool { return trigger == "issue-scheduled" || trigger == "issue-deadline" }

var cronRecurringKind = cronJobKind{
	id: cronRecurringID, label: "recurring child issue", triggers: allTriggers[:2],
	owns: cron.IsRecurringIssueJob,
	shown: func(f *cronForm, field cronField) bool {
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
			if strings.TrimSpace(f.val[cfParent]) == "" {
				return def, fieldErr(cfParent, "name the issue the children hang from")
			}
			parent = uidOf(f.val[cfParent])
		}
		title := strings.TrimSpace(f.val[cfTitle])
		if title == "" {
			return def, fieldErr(cfTitle, "must not be blank")
		}
		def.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: parent, Title: title, Body: strings.TrimSpace(f.val[cfBody])}
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
			case cfIssue:
				return f.val[cfPolicy] == "existing"
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
				f.val[cfIssue] = cmpOr(f.val[cfIssue], p.UID)
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
				if f.val[cfWorkflow] == "" {
					return def, fieldErr(cfWorkflow, "pick the workflow to run")
				}
				def.Action.WorkflowUID = f.val[cfWorkflow]
			} else if def.Action.Prompt = strings.TrimSpace(f.val[cfPrompt]); def.Action.Prompt == "" {
				return def, fieldErr(cfPrompt, "must not be blank")
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
			if def.TimeoutSeconds, err = cronSeconds(cfTimeout, f.val[cfTimeout], 0); err != nil {
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
		f.loadCommon(def)
		switch {
		case def.Issue != nil && cronIsDate(def.Trigger.Kind):
			f.val[cfReport] = def.Issue.UID
		case def.Issue != nil:
			f.val[cfIssue] = def.Issue.UID
		}
	},
	build: func(f *cronForm, uidOf func(string) string) (cron.JobDefinition, error) {
		def := f.base()
		def.Overlap, def.Action = "allow", cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
		f.applyNotification(&def)
		switch {
		case !cronIsDate(f.trigger):
			if strings.TrimSpace(f.val[cfIssue]) == "" {
				return def, fieldErr(cfIssue, "name the issue to notify about")
			}
			def.Issue = &cron.IssuePolicy{Kind: "existing", UID: uidOf(f.val[cfIssue])}
		case strings.TrimSpace(f.val[cfReport]) != "":
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
	load: func(f *cronForm, t cron.Trigger) { f.val[cfCron], f.val[cfZone] = t.Cron, cmpOr(t.Timezone, "UTC") },
	build: func(f *cronForm) (cron.Trigger, error) {
		expression := strings.TrimSpace(f.val[cfCron])
		if expression == "" {
			return cron.Trigger{}, fieldErr(cfCron, "enter 5 fields like 0 9 * * 1-5, or a descriptor like @daily")
		}
		zone := cmpOr(strings.TrimSpace(f.val[cfZone]), "UTC")
		if _, err := time.LoadLocation(zone); err != nil || zone == "Local" {
			return cron.Trigger{}, fieldErr(cfZone, "unknown time zone %q; use an IANA name like Europe/Berlin or UTC", zone)
		}
		return cron.Trigger{Kind: "cron", Cron: expression, Timezone: zone}, nil
	},
}

var cronIntervalTrigger = cronJobTrigger{
	id: "interval", label: "interval", fields: []cronField{cfInterval},
	load: func(f *cronForm, t cron.Trigger) { f.val[cfInterval] = strconv.FormatInt(t.IntervalSeconds, 10) },
	build: func(f *cronForm) (cron.Trigger, error) {
		seconds, err := cronSeconds(cfInterval, f.val[cfInterval], 1)
		if err != nil {
			return cron.Trigger{}, err
		}
		if seconds == 0 {
			return cron.Trigger{}, fieldErr(cfInterval, "enter seconds, or a duration like 15m or 2h")
		}
		return cron.Trigger{Kind: "interval", IntervalSeconds: seconds}, nil
	},
}

var cronOnceTrigger = cronJobTrigger{
	id: "once", label: "once", fields: []cronField{cfAt},
	load: func(f *cronForm, t cron.Trigger) { f.val[cfAt] = t.At },
	build: func(f *cronForm) (cron.Trigger, error) {
		at := strings.TrimSpace(f.val[cfAt])
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return cron.Trigger{}, fieldErr(cfAt, "enter an instant with its offset, like 2026-11-01T09:00:00+01:00 or 2026-11-01T08:00:00Z")
		}
		return cron.Trigger{Kind: "once", At: at}, nil
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
		lead, err := cronSeconds(cfLead, f.val[cfLead], 0)
		return cron.Trigger{Kind: "issue-deadline", LeadSeconds: lead}, err
	},
}

// cronSeconds reads an optional span as whole seconds: a number or a Go
// duration. Blank is zero. Values below min are refused unless min is 0.
func cronSeconds(field cronField, text string, min int64) (int64, error) {
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
	if n < min {
		return 0, fieldErr(field, "must be at least %d", min)
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

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	discard    bool
	conflict   *CronJobRecord
	workflows  []cronChoice
	taken      map[string]bool
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
func newCronForm(workflows []cronChoice, taken map[string]bool) (*cronForm, error) {
	minted, err := uid.New()
	if err != nil {
		return nil, err
	}
	f := &cronForm{uid: minted, create: true, kind: cronRecurringID, trigger: "cron", workflows: workflows, taken: taken}
	f.val[cfEnabled], f.val[cfCron], f.val[cfZone], f.val[cfInterval] = "yes", "0 9 * * *", "UTC", "3600"
	f.val[cfPolicy], f.val[cfOverlap], f.val[cfCatchup] = "per-run", "forbid", "latest"
	f.val[cfWorkflow] = ""
	f.finish()
	return f, nil
}

// cronFormFor is a form on an existing job. It reads the portable definition
// that was returned by the daemon, so nothing is lost on a round trip.
func cronFormFor(rec CronJobRecord, workflows []cronChoice, taken map[string]bool) (*cronForm, error) {
	f := &cronForm{uid: rec.UID, expected: rec.DefinitionEventUID, opened: rec.Definition, openedName: rec.Name, workflows: workflows, taken: taken}
	if err := f.load(rec); err != nil {
		return nil, err
	}
	f.finish()
	return f, nil
}

func (f *cronForm) load(rec CronJobRecord) error {
	def := rec.Definition
	f.val = [cfCount]string{}
	f.val[cfEnabled], f.val[cfZone], f.val[cfInterval], f.val[cfCron] = cronYesNoValue(def.Enabled), "UTC", "3600", "0 9 * * *"
	f.val[cfPolicy], f.val[cfOverlap], f.val[cfCatchup] = "per-run", "forbid", "latest"
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

func (f *cronForm) loadCommon(def cron.JobDefinition) {
	f.val[cfOverlap] = cmpOr(def.Overlap, "forbid")
	f.val[cfCatchup] = cmpOr(def.Catchup, "latest")
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
	def.CheckoutKey, def.SecretRefs, def.TimeoutSeconds, def.GraceSeconds, def.Options = "", nil, 0, 0, nil
	if f.kind == f.openedKind && !f.create {
		// Fields hidden by this kind or trigger keep their stored values.
		def.CheckoutKey, def.SecretRefs, def.TimeoutSeconds = f.opened.CheckoutKey, f.opened.SecretRefs, f.opened.TimeoutSeconds
		def.GraceSeconds, def.Options = f.opened.GraceSeconds, f.opened.Options
	}
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
	def.Action.Message = strings.TrimSpace(f.val[cfMessage])
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
	def.Catchup = cmpOr(f.val[cfCatchup], "latest")
	if !f.catchupShown() || def.Catchup != "skip" {
		return nil
	}
	grace, err := cronSeconds(cfGrace, f.val[cfGrace], 0)
	if err != nil {
		return err
	}
	if grace <= 0 && f.kind == cronRecurringID {
		return fieldErr(cfGrace, "skip needs a grace of 1 second or more")
	}
	def.GraceSeconds = grace
	return nil
}

func (f *cronForm) executePolicy(uidOf func(string) string) (*cron.IssuePolicy, error) {
	if f.val[cfPolicy] == "existing" {
		if strings.TrimSpace(f.val[cfIssue]) == "" {
			return nil, fieldErr(cfIssue, "name the issue every run belongs to")
		}
		return &cron.IssuePolicy{Kind: "existing", UID: uidOf(f.val[cfIssue])}, nil
	}
	policy := cron.IssuePolicy{Kind: "per-run", Title: strings.TrimSpace(f.val[cfTitle]), Body: strings.TrimSpace(f.val[cfBody])}
	if policy.Title == "" {
		return nil, fieldErr(cfTitle, "must not be blank")
	}
	for _, offset := range []struct {
		field cronField
		into  **int64
	}{{cfScheduled, &policy.ScheduledOffsetSeconds}, {cfDeadline, &policy.DeadlineOffsetSeconds}} {
		if strings.TrimSpace(f.val[offset.field]) == "" {
			continue
		}
		n, err := cronSeconds(offset.field, f.val[offset.field], -1<<62)
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
	var out []cronField
	for field := range cronField(cfCount) {
		if f.shown(field) {
			out = append(out, field)
		}
	}
	return out
}

func (f *cronForm) move(delta int) {
	order := f.order()
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
	_, _, err := f.build(func(string) string { return cronValidationUID })
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
	{"prompt", cfPrompt}, {"issue", cfIssue},
}

func (f *cronForm) attribute(err error) *cronFieldError {
	text := strings.TrimPrefix(err.Error(), cron.ErrInvalid.Error()+": ")
	for _, w := range cronErrorWords {
		if strings.Contains(text, w.word) && f.shown(w.field) {
			return &cronFieldError{w.field, text}
		}
	}
	return nil
}

// build reads the form into the job write it would send, or names the
// field that is wrong. uidOf turns an issue ref into its UID.
func (f *cronForm) build(uidOf func(string) string) (string, cron.JobDefinition, error) {
	f.syncEditor()
	name := strings.TrimSpace(f.val[cfName])
	switch {
	case name == "":
		return "", cron.JobDefinition{}, fieldErr(cfName, "must not be blank")
	case len(name) > 256:
		return "", cron.JobDefinition{}, fieldErr(cfName, "at most 256 bytes")
	case (f.create || name != f.openedName) && f.taken[strings.ToLower(name)]:
		return "", cron.JobDefinition{}, fieldErr(cfName, "another job already has this name")
	}
	trigger, err := f.triggerDef().build(f)
	if err != nil {
		return "", cron.JobDefinition{}, err
	}
	if !f.create && trigger.Kind == f.opened.Trigger.Kind && trigger.Timezone == "" {
		trigger.Timezone = f.opened.Trigger.Timezone
	}
	if cronIsDate(trigger.Kind) {
		if strings.TrimSpace(f.val[cfIssue]) == "" {
			return "", cron.JobDefinition{}, fieldErr(cfIssue, "name the issue whose date fires this job")
		}
		trigger.IssueUID = uidOf(f.val[cfIssue])
	}
	def, err := f.kindDef().build(f, uidOf)
	if err != nil {
		return "", cron.JobDefinition{}, err
	}
	def.Trigger = trigger
	if f.kind == cronRecurringID && trigger.Kind == "interval" && trigger.IntervalSeconds < cronMinNativeInterval {
		return "", def, fieldErr(cfInterval, "native intervals must be at least %d seconds", cronMinNativeInterval)
	}
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
