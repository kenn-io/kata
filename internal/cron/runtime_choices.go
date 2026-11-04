package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RuntimeChoicesFeature gates the additive choices wire member.
const RuntimeChoicesFeature = "cron_runtime_choices_v1"

var runtimeChoiceKeys = []string{"checkout_key", "options.agent", "options.model"}

// CloneRuntimeBeat gives every inventory and transport boundary independent data.
func CloneRuntimeBeat(b RuntimeBeat) RuntimeBeat {
	b.Features = slices.Clone(b.Features)
	if b.Choices != nil {
		copied := make(map[string][]string, len(b.Choices))
		for k, v := range b.Choices {
			copied[k] = slices.Clone(v)
		}
		b.Choices = copied
	}
	return b
}
func normalizeRuntimeChoices(choices map[string][]string) error {
	for key, values := range choices {
		if !slices.Contains(runtimeChoiceKeys, key) || values == nil || len(values) > 32 {
			return fmt.Errorf("invalid key or array %q", key)
		}
		slices.Sort(values)
		for i, v := range values {
			if !utf8.ValidString(v) || len(v) > 128 || strings.TrimSpace(v) == "" || strings.ContainsFunc(v, unicode.IsControl) || i > 0 && values[i-1] == v || key == "checkout_key" && !portableKey(v) {
				return fmt.Errorf("invalid value for %q", key)
			}
		}
	}
	return nil
}

// RuntimeMatches applies the exact, conjunctive selectors used by advisories.
func RuntimeMatches(r Runtime, pin, adapter string) bool {
	return (pin == "" || r.Label == pin) && (adapter == "" || r.Adapter == adapter)
}

// RuntimeChoices unions only matching live evidence. Present empty keys remain
// distinct from keys for which no runtime has advertised support.
func RuntimeChoices(entries []Runtime, def JobDefinition) map[string][]string {
	pin, adapter := runtimeSelectors(def.Options)
	out := map[string][]string{}
	for _, r := range entries {
		if r.Status != "live" || !RuntimeMatches(r, pin, adapter) {
			continue
		}
		for key, values := range r.Choices {
			if _, ok := out[key]; !ok {
				out[key] = []string{}
			}
			out[key] = append(out[key], values...)
		}
	}
	for key, values := range out {
		slices.Sort(values)
		out[key] = slices.Compact(values)
	}
	return out
}
func runtimeChoiceWarnings(live []Runtime, job RuntimeJob) []RuntimeWarning {
	choices := RuntimeChoices(live, job.Definition)
	values := map[string]string{"checkout_key": job.Definition.CheckoutKey}
	var fields map[string]jsontext.Value
	if json.Unmarshal(job.Definition.Options, &fields) == nil {
		for _, key := range []string{"agent", "model"} {
			var value string
			if json.Unmarshal(fields[key], &value) == nil {
				values["options."+key] = value
			}
		}
	}
	out := []RuntimeWarning{}
	for _, key := range runtimeChoiceKeys {
		value := values[key]
		offered, known := choices[key]
		if known && strings.TrimSpace(value) != "" && !slices.Contains(offered, value) {
			out = append(out, RuntimeWarning{Code: "choice_unknown", JobUID: job.UID, Choice: key, Value: value, Message: fmt.Sprintf("value %q is not advertised for %s by matching live runtimes", value, key)})
		}
	}
	return out
}
