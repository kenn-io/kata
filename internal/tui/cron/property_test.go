package cron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"testing"

	"github.com/oklog/ulid/v2"

	native "go.kenn.io/kata/internal/cron"
)

// Editing a portable field preserves every opaque option, including all int64 values.
func FuzzCronDraftPreservesOpaqueOptions(f *testing.F) {
	f.Add(int64(9007199254740993), "example-option", "Updated summary")
	f.Add(int64(-9223372036854775808), "", "")
	f.Add(int64(9223372036854775807), "例\n\x1b[31m", "Summary")
	f.Fuzz(func(t *testing.T, number int64, text, prompt string) {
		options, err := json.Marshal(map[string]any{"number": json.Number(strconv.FormatInt(number, 10)), "text": text})
		if err != nil {
			t.Fatal(err)
		}
		j := formTestJob()
		var raw map[string]jsontext.Value
		if err = json.Unmarshal(j.Definition, &raw); err != nil {
			t.Fatal(err)
		}
		raw["options"] = options
		j.Definition, err = json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		m := newJobForm(context.Background(), nil, &j, nil, editorContext{})
		m.Set("prompt", prompt)
		edited, err := m.definition()
		if err != nil {
			t.Fatal(err)
		}
		var after map[string]jsontext.Value
		if err = json.Unmarshal(edited, &after); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(options, after["options"]) {
			t.Fatalf("opaque values changed: %s → %s", options, after["options"])
		}
	})
}

// Navigation producers use decimal project IDs, ULIDs and portable step keys.
func FuzzCronDetailIdentity(f *testing.F) {
	f.Add(uint8(0), uint64(7), "example")
	f.Add(uint8(1), uint64(8), "结果")
	f.Fuzz(func(t *testing.T, kind uint8, projectID uint64, seed string) {
		kinds := []string{"run", "step", "used-by", "job"}
		k := kinds[kind%4]
		project := strconv.FormatUint(projectID%9223372036854775807+1, 10)
		digest := sha256.Sum256([]byte(seed))
		var id ulid.ULID
		copy(id[:], digest[:16])
		uid := id.String()
		if k == "step" {
			uid = seed
			workflow := native.WorkflowDefinition{Version: 1, Steps: []native.WorkflowStep{{Key: uid, Kind: "command", Command: "true"}}}
			if workflow.Validate() != nil {
				uid = fmt.Sprintf("step_%x", digest[:16])
			}
		}
		encoded := detailItemKey(k, project, uid)
		gotKind, gotProject, gotUID, ok := parseDetailItemKey(encoded)
		if !ok || gotKind != k || gotProject != project || gotUID != uid {
			t.Fatal("navigation identity failed roundtrip")
		}
	})
}
