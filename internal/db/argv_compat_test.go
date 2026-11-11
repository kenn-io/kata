package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

const argvCompatFeature = "cron_workflow_argv_v1"

var argvCompatEvents = []string{"cron.workflow.created", "cron.workflow.updated", "cron.workflow.deleted", "cron.workflow.restored", "cron.workflow.snapshot"}

// The reserved kind requires an additional peer capability even when the
// argument member is absent or invalid. Classification is not validation.
func TestArgvCompatPayloadFeatures(t *testing.T) {
	for _, kind := range argvCompatEvents {
		for _, step := range []string{`{"kind":"argv","argv":["printf","%s",""]}`, `{"kind":"argv"}`, `{"kind":"argv","argv":null}`} {
			t.Run(kind+"/"+step, func(t *testing.T) {
				payload := `{"definition":{"version":1,"steps":[` + step + `]}}`
				features, err := db.PayloadRequiredFeatures(kind, payload)
				require.NoError(t, err)
				require.Equal(t, db.MergeEventFeatures(db.CronEventFeature, argvCompatFeature), features)
				version, outbound, err := db.FederationEventPayloadWireVersion(kind, jsontext.Value(payload))
				require.NoError(t, err)
				require.Equal(t, 31, version)
				require.Equal(t, features, outbound)
				require.ErrorIs(t, db.RequireEventFeatures(db.CronEventFeature, features), db.ErrUnsupportedEventFeatures)
			})
		}
	}
	require.ErrorIs(t, db.RequireEventFeatures(db.SupportedEventFeatures, argvCompatFeature), db.ErrUnsupportedEventFeatures, "a reserved feature is not decoder support")
}

func TestArgvCompatMalformedPayload(t *testing.T) {
	for _, payload := range []string{`{`, `[]`, `null`, `{} {}`, `{"definition":null}`, `{"definition":[]}`, `{"definition":"argv"}`, `{"definition":{"steps":null}}`, `{"definition":{"steps":"argv"}}`, `{"definition":{"steps":[false]}}`, `{"definition":{"steps":[null]}}`, `{"definition":{"steps":[{"kind":42}]}}`, `{"definition":{"steps":[{"kind":null}]}}`, `{"definition":{"steps":[{"kind":"argv"},false]}}`, `{"definition":{"steps":[{"kind":"command","kind":"argv"}]}}`, `{"definition":{"steps":[{"kind":"command","\u006bind":"argv"}]}}`} {
		for _, kind := range argvCompatEvents {
			t.Run(kind+"/"+payload, func(t *testing.T) {
				_, err := db.PayloadRequiredFeatures(kind, payload)
				require.Error(t, err)
				_, _, err = db.FederationEventPayloadWireVersion(kind, jsontext.Value(payload))
				require.Error(t, err)
			})
		}
	}
}

func TestArgvCompatNonReservedKinds(t *testing.T) {
	for _, step := range []string{`{}`, `{"kind":""}`, `{"kind":"future"}`, `{"kind":"Argv"}`, `{"kind":"argv "}`, `{"kind":"\u0430rgv"}`} {
		payload := `{"definition":{"steps":[` + step + `]}}`
		features, err := db.PayloadRequiredFeatures("cron.workflow.updated", payload)
		require.NoError(t, err)
		require.Equal(t, db.CronEventFeature, features)
	}
}

func TestArgvCompatLegacyPayloads(t *testing.T) {
	for _, payload := range []string{"", `{}`, `{"definition":{"steps":[{"kind":"command","command":"echo argv","options":{"kind":"argv"}}]}}`, `{"definition":{"steps":[{"kind":"prompt","prompt":"argv"}],"options":{"steps":[{"kind":"argv"}]}}}`} {
		features, err := db.PayloadRequiredFeatures("cron.workflow.updated", payload)
		require.NoError(t, err)
		require.Equal(t, db.CronEventFeature, features)
	}
	features, err := db.PayloadRequiredFeatures("project.metadata_updated", `{"definition":{"steps":[{"kind":"argv"}]}}`)
	require.NoError(t, err)
	require.Empty(t, features)
}

func TestArgvCompatClassificationIsNotSizeValidation(t *testing.T) {
	const definition = `{"version":1,"steps":[{"key":"literal","kind":"argv"}]}`
	for _, size := range []int{cron.DefinitionLimit - 1, cron.DefinitionLimit, cron.DefinitionLimit + 1} {
		padded := definition[:len(definition)-1] + strings.Repeat(" ", size-len(definition)) + `}`
		require.Len(t, padded, size)
		payload := `{"definition":` + padded + `}`
		features, err := db.PayloadRequiredFeatures("cron.workflow.updated", payload)
		require.NoError(t, err)
		require.Equal(t, db.MergeEventFeatures(db.CronEventFeature, argvCompatFeature), features)
	}
}

func argvCompatEscape(value string, mask uint64) string {
	var encoded strings.Builder
	for i, c := range value {
		if mask&(uint64(1)<<i) != 0 {
			fmt.Fprintf(&encoded, `\u%04x`, c)
		} else {
			encoded.WriteRune(c)
		}
	}
	return encoded.String()
}

// JSON escaping is semantic: neither member names nor the discriminator can
// conceal the reserved feature from either classifier entrypoint.
func TestArgvCompatEscapingProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mask := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		kind := hegel.Draw(ht, hegel.SampledFrom(argvCompatEvents))
		payload := `{"definition":{"steps":[{"kind":"argv"}]}}`
		for _, word := range []string{"definition", "steps", "kind", "argv"} {
			payload = strings.ReplaceAll(payload, `"`+word+`"`, `"`+argvCompatEscape(word, mask)+`"`)
			mask = mask>>8 | mask<<56
		}
		want := db.MergeEventFeatures(db.CronEventFeature, argvCompatFeature)
		features, err := db.PayloadRequiredFeatures(kind, payload)
		if err != nil || features != want {
			ht.Fatalf("escaped payload classified as %q, %v; want %q: %s", features, err, want, payload)
		}
		_, features, err = db.FederationEventPayloadWireVersion(kind, jsontext.Value(payload))
		if err != nil || features != want {
			ht.Fatalf("outbound escaped payload classified as %q, %v; want %q", features, err, want)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestArgvCompatOpaqueTextProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().Codec("utf-8"))
		kind := hegel.Draw(ht, hegel.SampledFrom(argvCompatEvents))
		payload, err := json.Marshal(map[string]any{"definition": map[string]any{"options": map[string]any{"kind": "argv", "note": text}, "steps": []map[string]any{{"kind": "command", "command": "argv " + text}}}})
		if err != nil {
			ht.Fatalf("marshal valid Unicode: %v", err)
		}
		features, err := db.PayloadRequiredFeatures(kind, string(payload))
		if err != nil || features != db.CronEventFeature {
			ht.Fatalf("opaque text affected workflow capability: %q, %v", features, err)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestArgvCompatStepPositionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kinds := hegel.Draw(ht, hegel.Lists(hegel.Booleans()))
		position := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)) % uint64(len(kinds)+1)
		steps := make([]map[string]any, len(kinds)+1)
		for i := range steps {
			steps[i] = map[string]any{"kind": "command", "command": "true"}
		}
		steps[position] = map[string]any{"kind": "argv", "argv": nil}
		for i, prompt := range kinds {
			if prompt && uint64(i) != position {
				steps[i] = map[string]any{"kind": "prompt", "prompt": "argv"}
			}
		}
		payload, err := json.Marshal(map[string]any{"definition": map[string]any{"steps": steps}})
		if err != nil {
			ht.Fatalf("marshal steps: %v", err)
		}
		for _, kind := range argvCompatEvents {
			features, err := db.PayloadRequiredFeatures(kind, string(payload))
			want := db.MergeEventFeatures(db.CronEventFeature, argvCompatFeature)
			if err != nil || features != want {
				ht.Fatalf("argv at position %d of %d classified as %q, %v", position, len(steps), features, err)
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
