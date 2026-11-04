package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// Responsibility separates configured preferences from reported execution
// evidence. Unknown assignment does not mean that no runtime exists.
type Responsibility struct {
	Actor         *string `json:"actor"`
	Runtime       *string `json:"runtime"`
	RuntimeStatus string  `json:"runtime_status"`
	Adapter       *string `json:"adapter"`
	Executor      *string `json:"executor"`
}

// DefinitionResponsibility never treats creator provenance or opaque options
// as an execution assignment. Native jobs identify a mechanism, not a host.
func DefinitionResponsibility(options jsontext.Value, native bool) Responsibility {
	r := Responsibility{RuntimeStatus: "unknown"}
	var fields map[string]jsontext.Value
	if json.Unmarshal(options, &fields) == nil {
		r.Adapter = preference(fields["adapter"])
		r.Executor = preference(fields["executor"])
	}
	if native {
		r.Runtime, r.RuntimeStatus = new("owning daemon"), "daemon"
	}
	return r
}

func preference(raw jsontext.Value) *string {
	var value string
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// RunResponsibility preserves self-reported labels without certifying runtime
// identity or assigning that runtime any future work.
func RunResponsibility(actor string, executorLabel *string) Responsibility {
	r := Responsibility{Actor: &actor, RuntimeStatus: "unreported"}
	if executorLabel != nil && strings.TrimSpace(*executorLabel) != "" {
		r.Runtime, r.RuntimeStatus = executorLabel, "reported"
	}
	return r
}
