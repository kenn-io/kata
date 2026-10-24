package records

import (
	"encoding/json/jsontext"
	"testing"
)

func TestSameDefinitionRetainsNativeMeaning(t *testing.T) {
	const job = `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summarize"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993}}`
	base := Job{UID: "example-job", Name: "Example", Definition: jsontext.Value(job)}
	for _, tc := range []struct{ name, definition string }{
		{"exact number", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summarize"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip","options":{"number":9007199254740992}}`},
		{"explicit zero offset", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summarize"},"issue":{"kind":"per-run","title":"Summary","scheduled_offset_seconds":0},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993}}`},
		{"opaque null", `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Summarize"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993,"other":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			other.Definition = jsontext.Value(tc.definition)
			if SameDefinition(base, other) {
				t.Fatal("distinct document accepted as committed retry")
			}
		})
	}
}
