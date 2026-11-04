package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/cron"
)

func cronRuntimeFieldKey(field cronField) string {
	switch field {
	case cfCheckout:
		return "checkout_key"
	case cfAgent:
		return "options.agent"
	case cfModel:
		return "options.model"
	}
	return ""
}
func cronRuntimeOptionFields(raw string) (map[string]jsontext.Value, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]jsontext.Value{}, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("options must be an object")
	}
	return fields, nil
}

// cronSetRuntimeOption changes only one member. Raw JSON values preserve large
// integers and other adapter-owned data; callers invoke it only for actual edits.
func cronSetRuntimeOption(raw, key, value string) (string, error) {
	fields, err := cronRuntimeOptionFields(raw)
	if err != nil {
		return raw, err
	}
	var before string
	if prior, present := fields[key]; present {
		if json.Unmarshal(prior, &before) != nil {
			return raw, fmt.Errorf("option is not a string")
		}
	} else if value == "" {
		return raw, nil
	}
	if before == value && value != "" {
		return raw, nil
	}
	if value == "" {
		delete(fields, key)
	} else {
		encoded, e := json.Marshal(value)
		if e != nil {
			return raw, e
		}
		fields[key] = encoded
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return raw, err
	}
	return string(encoded), nil
}
func (f *cronForm) runtimeSelection(field cronField) (string, bool) {
	if field == cfCheckout {
		return f.val[field], f.val[field] != ""
	}
	fields, err := cronRuntimeOptionFields(f.val[cfOptions])
	if err != nil {
		return "", false
	}
	raw, present := fields[strings.TrimPrefix(cronRuntimeFieldKey(field), "options.")]
	var value string
	if !present || strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
func (f *cronForm) runtimeCurrent(field cronField) string {
	value, _ := f.runtimeSelection(field)
	return value
}
func (f *cronForm) runtimeDisplay(field cronField) string {
	value, present := f.runtimeSelection(field)
	if !present {
		return "(unset)"
	}
	if !slices.Contains(f.runtimeChoices[cronRuntimeFieldKey(field)], value) {
		if value == "" {
			return `"" (not advertised)`
		}
		return value + " (not advertised)"
	}
	return value
}
func (f *cronForm) runtimeFieldUsable(field cronField) bool {
	fields, err := cronRuntimeOptionFields(f.val[cfOptions])
	if err != nil {
		return false
	}
	if field == cfCheckout {
		return true
	}
	raw, present := fields[strings.TrimPrefix(cronRuntimeFieldKey(field), "options.")]
	if !present {
		return true
	}
	var value string
	return len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &value) == nil
}
func (f *cronForm) runtimeFieldShown(field cronField) bool {
	if !f.runtimeFieldUsable(field) {
		return false
	}
	_, advertised := f.runtimeChoices[cronRuntimeFieldKey(field)]
	return advertised || f.runtimeFree[field]
}
func (f *cronForm) refreshRuntimeChoices() {
	var entries []cron.Runtime
	if f.runtimeInventory != nil {
		entries = f.runtimeInventory.Runtimes
	}
	f.runtimeChoices = cron.RuntimeChoices(entries, cron.JobDefinition{Options: jsontext.Value(f.val[cfOptions])})
}
func (f *cronForm) setRuntimeInventory(inventory *cron.RuntimeInventory) {
	f.syncEditor()
	f.runtimeInventory = inventory
	f.refreshRuntimeChoices()
	// A disappearing picker must not leave focus on a hidden field.
	if !f.shown(f.focus) {
		f.focusField(cfOptions)
	}
}

type cronRuntimeActionKind int

const (
	cronRuntimeUnset cronRuntimeActionKind = iota
	cronRuntimeValue
	cronRuntimeOther
)

type cronRuntimeAction struct {
	kind         cronRuntimeActionKind
	value, label string
}

func (f *cronForm) runtimeActions(field cronField) []cronRuntimeAction {
	values := f.runtimeChoices[cronRuntimeFieldKey(field)]
	out := []cronRuntimeAction{{kind: cronRuntimeUnset, label: "(unset)"}}
	for _, v := range values {
		out = append(out, cronRuntimeAction{kind: cronRuntimeValue, value: v, label: v})
	}
	current, present := f.runtimeSelection(field)
	if present && !slices.Contains(values, current) {
		label := current + " (not advertised)"
		if current == "" {
			label = `"" (not advertised)`
		}
		out = append(out, cronRuntimeAction{kind: cronRuntimeValue, value: current, label: label})
	}
	return append(out, cronRuntimeAction{kind: cronRuntimeOther, label: "other…"})
}
func (f *cronForm) applyRuntimeAction(field cronField, action cronRuntimeAction) {
	if action.kind == cronRuntimeOther {
		f.runtimeFree[field] = true
		f.focusField(field)
		return
	}
	value := action.value
	// The current empty string is a real value; only the unset action deletes it.
	if action.kind == cronRuntimeValue && value == "" {
		return
	}
	if action.kind == cronRuntimeUnset {
		value = ""
	}
	if field == cfCheckout {
		f.val[field] = value
	} else {
		key := strings.TrimPrefix(cronRuntimeFieldKey(field), "options.")
		raw, err := cronSetRuntimeOption(f.val[cfOptions], key, value)
		if err == nil {
			f.val[cfOptions] = raw
		}
	}
	// Selection changes the displayed value, never leaves a stale text editor.
	f.edInit = f.current(field)
	if f.ed.activeField() != nil && f.focus == field {
		f.ed.activeField().setValue(f.edInit)
	}
	f.validate()
}
func (f *cronForm) stepRuntime(field cronField, delta int) {
	actions := f.runtimeActions(field)
	current, present := f.runtimeSelection(field)
	at := slices.IndexFunc(actions, func(a cronRuntimeAction) bool {
		return a.kind == cronRuntimeUnset && !present || a.kind == cronRuntimeValue && present && a.value == current
	})
	at = max(at, 0)
	f.applyRuntimeAction(field, actions[(at+delta+len(actions))%len(actions)])
}
func (f *cronForm) runtimeNote(field cronField) string {
	key := cronRuntimeFieldKey(field)
	values, known := f.runtimeChoices[key]
	if value := f.runtimeCurrent(field); known && value != "" && !slices.Contains(values, value) {
		return " (not advertised)"
	}
	return ""
}
