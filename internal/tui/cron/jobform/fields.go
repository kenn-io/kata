package jobform

import (
	"encoding/json/v2"
	"fmt"
	"sort"
	"strings"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

// FieldKind identifies a source form control.
type FieldKind uint8

// Form controls retain the source editing behavior.
const (
	Text FieldKind = iota
	TextArea
	Bool
	Choice
	ReadOnly
)

// Context supplies portable catalog and issue-reference display data.
type Context struct {
	IssueReference func(string) string
	Workflows      []records.Workflow
	Project        string
	Persisted      bool
}

// Draft keeps original JSON separate from editable field text.
type Draft struct {
	Raw    map[string]any
	Values map[string]string
	Dirty  map[string]bool
}

// Field describes a control and its validation choices.
type Field struct {
	Key, Label, Help, ReadOnlyReason string
	Kind                             FieldKind
	Choices                          []string
}

var fieldPaths = map[string]string{"description": "options.description", "tags": "options.tags", "input": "options.input", "enabled": "enabled", "trigger": "trigger.kind", "timezone": "trigger.timezone", "prompt": "action.prompt", "workflow": "action.workflow_uid", "recipient": "action.recipient", "message": "action.message", "issue_kind": "issue.kind", "issue": "issue.uid", "parent": "issue.parent_uid", "title": "issue.title", "body": "issue.body", "deadline": "issue.deadline_offset_seconds", "overlap": "overlap", "catchup": "catchup", "grace": "grace_seconds", "timeout": "timeout_seconds", "checkout": "checkout_key", "profile": "options.agent", "run_parent": "options.run_parent", "secret_refs": "secret_refs", "trigger_issue": "trigger.issue_uid", "lead": "trigger.lead_seconds", "scheduled_offset": "issue.scheduled_offset_seconds"}

func rawValue(raw map[string]any, path string) any {
	var v any = raw
	for key := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

// Path returns the definition path for an editable key and trigger kind.
func Path(key, trigger string) string {
	if key == "schedule" {
		switch trigger {
		case "cron":
			return "trigger.cron"
		case "interval":
			return "trigger.interval_seconds"
		case "once":
			return "trigger.at"
		}
		return ""
	}
	return fieldPaths[key]
}

// ModeOf derives the form action from a raw definition.
func ModeOf(raw map[string]any) string {
	if rawValue(raw, "action.kind") == "notify" {
		if rawValue(raw, "issue.kind") == "per-run" {
			return "recurring"
		}
		return "notify"
	}
	if uid := rawValue(raw, "action.workflow_uid"); uid != nil && uid != "" {
		return "workflow"
	}
	return "prompt"
}

// NewDraft hydrates editable text without normalizing the stored definition.
func NewDraft(raw map[string]any, name string) Draft {
	d := Draft{Raw: raw, Values: map[string]string{"name": name, "mode": ModeOf(raw)}, Dirty: map[string]bool{}}
	for key, path := range fieldPaths {
		v := rawValue(raw, path)
		if v == nil {
			continue
		}
		switch key {
		case "tags":
			b, _ := json.Marshal(v)
			var tags []string
			_ = json.Unmarshal(b, &tags)
			d.Values[key] = strings.Join(tags, ", ")
		case "secret_refs":
			b, _ := json.Marshal(v)
			var refs map[string]string
			_ = json.Unmarshal(b, &refs)
			d.Values[key] = secretText(refs)
		case "grace", "timeout", "deadline", "lead":
			text := fmt.Sprint(v)
			d.Values[key] = text
			if n, err := ParseSeconds(key, text, key == "deadline"); err == nil {
				d.Values[key] = SecondsText(n)
			}
		default:
			d.Values[key] = fmt.Sprint(v)
		}
	}
	if v := rawValue(raw, Path("schedule", d.Values["trigger"])); v != nil {
		d.Values["schedule"] = fmt.Sprint(v)
		if d.Values["trigger"] == "interval" {
			if n, err := ParseSeconds("interval", d.Values["schedule"], false); err == nil {
				d.Values["schedule"] = SecondsText(n)
			}
		}
	}
	return d
}

func value(d Draft, key string) string { return d.Values[key] }

var knownOptions = map[string]bool{}

// Fields lists native controls for the selected action and trigger.
func Fields(d Draft, c Context) []Field {
	out := make([]Field, 0, 24)
	add := func(key, label string, kind FieldKind, help string, choices ...string) {
		out = append(out, Field{Key: key, Label: label, Kind: kind, Help: help, Choices: choices})
	}
	mode := value(d, "mode")
	lockedRecurring := c.Persisted && ModeOf(d.Raw) == "recurring"
	execute := mode == "prompt" || mode == "workflow"
	add("name", "Name", Text, "required; must be unique in the project")
	if mode != "recurring" {
		add("description", "Description", ReadOnly, "preserved; edit the definition with kata cron job")
		add("tags", "Tags", ReadOnly, "preserved; edit the definition with kata cron job")
	}
	add("mode", "Action", Choice, "choose the job action", "prompt", "workflow", "notify", "recurring")
	if lockedRecurring {
		out[len(out)-1].Kind = ReadOnly
		out[len(out)-1].ReadOnlyReason = "native recurring ownership is immutable"
	}
	switch mode {
	case "prompt":
		add("prompt", "Prompt", TextArea, "required")
	case "workflow":
		var choices []string
		for _, w := range c.Workflows {
			if w.DeletedAt == nil {
				choices = append(choices, w.UID)
			}
		}
		sort.Slice(choices, func(i, j int) bool {
			return workflowName(c, choices[i])+choices[i] < workflowName(c, choices[j])+choices[j]
		})
		add("workflow", "Workflow", Choice, "choose an available workflow", choices...)
		add("input", "Input", ReadOnly, "preserved; edit the definition with kata cron job")
	case "notify":
		add("recipient", "Recipient", Text, "exact recipient or current-owner-or-author")
		add("message", "Message", TextArea, "notification text")
	case "recurring":
		add("recipient", "Recipient", Text, "exact recipient or current-owner-or-author")
		add("parent", "Parent issue", Text, "parent issue reference; resolved at save")
		if lockedRecurring {
			out[len(out)-1].Kind = ReadOnly
			out[len(out)-1].ReadOnlyReason = "native parent binding is immutable"
		}
		add("title", "Child title", Text, "required")
		add("body", "Child body", TextArea, "the child's instruction")
		add("message", "Message", TextArea, "optional notification text")
	}
	triggers := []string{"manual", "cron", "interval", "once", "issue-scheduled", "issue-deadline"}
	if mode == "recurring" {
		triggers = []string{"cron", "interval"}
	}
	add("trigger", "Schedule", Choice, "choose the schedule", triggers...)
	switch value(d, "trigger") {
	case "cron":
		add("schedule", "Cron", Text, "5-field cron, e.g. 0 7 * * *")
	case "interval":
		add("schedule", "Interval", Text, "e.g. 1h, 30m · whole seconds also accepted")
	case "once":
		add("schedule", "Run at", Text, "RFC3339 or local wall time")
	case "issue-scheduled":
		add("trigger_issue", "Issue reference", Text, "issue with a scheduled date; resolved at save")
	case "issue-deadline":
		add("trigger_issue", "Issue reference", Text, "issue with a deadline; resolved at save")
		add("lead", "Lead", Text, "non-negative duration before the deadline")
	}
	add("timezone", "Timezone", Text, "IANA timezone; blank means UTC")
	add("catchup", "Catchup", Choice, "missed occurrences", "latest", "all", "skip")
	add("grace", "Grace", Text, "duration; blank removes the override")
	add("overlap", "Overlap", Choice, "allow or forbid overlapping runs", "forbid", "allow")
	if mode == "recurring" {
		out[len(out)-1].Kind = ReadOnly
		out[len(out)-1].ReadOnlyReason = "recurring jobs require allow"
	}
	if execute {
		add("timeout", "Timeout", Text, "duration; blank preserves no explicit timeout")
		add("checkout", "Checkout", Text, "portable key; resolved by the consumer")
		add("issue_kind", "Issue", Choice, "issue policy", "per-run", "existing")
		if value(d, "issue_kind") == "per-run" {
			add("title", "Issue title", Text, "blank uses job name")
			add("body", "Issue body", TextArea, "run issue body")
			add("deadline", "Deadline offset", Text, "signed duration; blank removes the offset")
		} else {
			add("issue", "Existing issue", Text, "issue reference; resolved at save")
		}
		add("secret_refs", "Secret refs", Text, "portable VAR=name pairs; no secret values")
	} else if mode == "notify" {
		add("issue", "Existing issue", Text, "issue reference; resolved at save")
	}
	add("other_options", "Other options", ReadOnly, "all adapter options are preserved")
	out[len(out)-1].ReadOnlyReason = "preserved; edit the definition with kata cron job"
	if rawValue(d.Raw, "issue.scheduled_offset_seconds") != nil {
		add("scheduled_offset", "Scheduled offset", ReadOnly, "preserved existing offset")
		out[len(out)-1].ReadOnlyReason = "preserved"
	}
	add("enabled", "Enabled", Bool, "enable scheduling")
	return out
}
func workflowName(c Context, uid string) string {
	if uid == "" {
		return "select a workflow"
	}
	for _, w := range c.Workflows {
		if w.UID == uid {
			return w.Name
		}
	}
	return "workflow unavailable"
}

// Get returns the display and editable text for one field.
func Get(f Field, d Draft, c Context) (display, edit string) {
	edit = value(d, f.Key)
	display = edit
	switch f.Key {
	case "overlap":
		if value(d, "mode") == "recurring" {
			display = "allow"
			edit = "allow"
		}
	case "schedule":
		if value(d, "trigger") == "once" && edit != "" {
			if s, _, err := RunAtText(edit, value(d, "timezone")); err == nil {
				display = s
			}
		}
	case "workflow":
		display = workflowName(c, edit)
	case "issue", "parent", "trigger_issue", "run_parent":
		display = ""
		if c.IssueReference != nil {
			display = c.IssueReference(edit)
		}
		if display == "" {
			if i := strings.Index(edit, "#"); i > 0 {
				display = edit
			} else if short := strings.TrimPrefix(edit, "#"); short != "" && len(short) < 26 && c.Project != "" {
				display = c.Project + "#" + short
			}
		}

	case "other_options":
		if opts, ok := d.Raw["options"].(map[string]any); ok {
			var keys []string
			for key := range opts {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			display = strings.Join(keys, ", ")
			if display != "" {
				display += " (preserved)"
			}
		}
	}
	if f.Kind == Bool {
		display = "no"
		if edit == "true" {
			display = "yes"
		}
	}
	if display == "" {
		display = "—"
	}
	return
}

// Set validates and updates one portable field without rewriting the original JSON.
func Set(f Field, d *Draft, _ Context, text string) error {
	if f.Kind == ReadOnly {
		return fmt.Errorf("%s is read-only: %s", f.Label, f.ReadOnlyReason)
	}
	canonical := text
	switch f.Key {
	case "name":
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("name: required")
		}
		canonical = strings.TrimSpace(text)
	case "description":
		if err := defs.ValidateJobDescription(text); err != nil {
			return err
		}
	case "tags":
		tags, err := canonicalTags(text)
		if err != nil {
			return err
		}
		canonical = strings.Join(tags, ", ")
	case "secret_refs":
		refs, err := ParseSecretRefs(text)
		if err != nil {
			return err
		}
		canonical = secretText(refs)
	case "timezone":
		if _, err := jobLocation(text); err != nil {
			return err
		}
	case "schedule":
		switch value(*d, "trigger") {
		case "cron":
			if _, err := defs.CronExpression(defs.Trigger{Kind: "cron", Cron: text, Timezone: value(*d, "timezone")}); err != nil {
				return fmt.Errorf("cron: %w", err)
			}
		case "once":
			var err error
			canonical, err = ParseRunAt(text, value(*d, "schedule"), value(*d, "timezone"))
			if err != nil {
				return err
			}
		case "interval":
			n, err := ParseSeconds("interval", text, false)
			if err != nil {
				return err
			}
			if n <= 0 || value(*d, "mode") == "recurring" && n < 60 {
				return fmt.Errorf("interval: must be positive; recurring requires at least 1m")
			}
			canonical = SecondsText(n)
		}
	case "timeout", "grace", "deadline", "lead":
		if strings.TrimSpace(text) != "" {
			n, err := ParseSeconds(f.Key, text, f.Key == "deadline")
			if err != nil {
				return err
			}
			canonical = SecondsText(n)
		} else {
			canonical = ""
		}
	case "prompt":
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("prompt: required")
		}

	}
	if f.Kind == Choice {
		valid := false
		for _, choice := range f.Choices {
			valid = valid || canonical == choice
		}
		if !valid {
			return fmt.Errorf("%s: choose an available value", f.Label)
		}
	}
	if canonical == value(*d, f.Key) {
		return nil
	}
	if d.Values == nil {
		d.Values = map[string]string{}
	}
	if d.Dirty == nil {
		d.Dirty = map[string]bool{}
	}
	if f.Key == "trigger" {
		d.Values["schedule:"+value(*d, "trigger")] = value(*d, "schedule")
		d.Values["schedule"] = d.Values["schedule:"+canonical]
	}
	d.Values[f.Key] = canonical
	d.Dirty[f.Key] = true
	return nil
}

// ParseSecretRefs validates names only; it never resolves secret sources.
func ParseSecretRefs(text string) (map[string]string, error) {
	refs := map[string]string{}
	if strings.TrimSpace(text) == "" {
		return refs, nil
	}
	for part := range strings.SplitSeq(text, ",") {
		variable, name, ok := strings.Cut(strings.TrimSpace(part), "=")
		variable, name = strings.TrimSpace(variable), strings.TrimSpace(name)
		if !ok || !defs.PortableKey(variable) || !defs.PortableKey(name) {
			return nil, fmt.Errorf("secret refs: invalid portable names")
		}
		if _, exists := refs[variable]; exists {
			return nil, fmt.Errorf("secret refs: duplicate variable %s", variable)
		}
		refs[variable] = name
	}
	return refs, nil
}
func secretText(refs map[string]string) string {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+refs[key])
	}
	return strings.Join(parts, ", ")
}

func canonicalTags(text string) ([]string, error) {
	var tags []string
	seen := map[string]bool{}
	if strings.TrimSpace(text) != "" {
		for tag := range strings.SplitSeq(text, ",") {
			tag = strings.TrimSpace(tag)
			if !seen[tag] {
				tags = append(tags, tag)
				seen[tag] = true
			}
		}
	}
	if err := defs.ValidateJobTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// Encode returns a field's stored definition path and value. Nil deletes the field.
// It leaves form policy and preservation of untouched fields to the caller.
func Encode(key string, d Draft, _ Context) (path string, v any, err error) {
	path = Path(key, value(d, "trigger"))
	if path == "" {
		return "", nil, nil
	}
	text := value(d, key)
	switch key {
	case "schedule":
		switch value(d, "trigger") {
		case "interval":
			n, err := ParseSeconds("interval", text, false)
			return path, n, err
		case "once":
			original, _ := rawValue(d.Raw, "trigger.at").(string)
			stored, err := ParseRunAt(text, original, value(d, "timezone"))
			return path, stored, err
		default:
			return path, text, nil
		}
	case "enabled":
		return path, text == "true", nil
	case "deadline", "grace", "timeout", "lead":
		if strings.TrimSpace(text) == "" {
			return path, nil, nil
		}
		n, err := ParseSeconds(key, text, key == "deadline")
		return path, n, err
	case "tags":
		tags, err := canonicalTags(text)
		if err != nil {
			return path, nil, err
		}
		if len(tags) == 0 {
			return path, nil, nil
		}
		return path, tags, nil
	case "secret_refs":
		refs, err := ParseSecretRefs(text)
		if err != nil {
			return path, nil, err
		}
		if len(refs) == 0 {
			return path, nil, nil
		}
		return path, refs, nil
	default:
		if text == "" {
			return path, nil, nil
		}
		return path, text, nil
	}
}
