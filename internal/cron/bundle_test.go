package cron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const bundleTestUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const bundleWorkflowUID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

func testBundle() Bundle {
	return Bundle{Format: "kata.cron.bundle", Version: 1, Source: BundleSource{ProjectUID: bundleTestUID, ProjectName: "source-project", ExportedAt: "2026-11-04T00:00:00Z"}, Jobs: []BundleDefinition{}, Workflows: []BundleDefinition{{UID: bundleWorkflowUID, DefinitionEventUID: bundleTestUID, Name: "Inspect", Definition: jsontext.Value(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}`)}}}
}

func TestBundleRejectsMalformedEnvelope(t *testing.T) {
	b := testBundle()
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	for name, input := range map[string][]byte{
		"null": []byte("null"), "array": []byte("[]"), "trailing": append(bytes.Clone(raw), []byte(" {}")...),
		"unknown":          []byte(strings.Replace(string(raw), `"version":1`, `"unknown":true,"version":1`, 1)),
		"duplicate member": []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)),
		"oversize":         bytes.Repeat([]byte(" "), BundleLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBundle(input)
			require.Error(t, err)
			if name == "unknown" {
				require.ErrorIs(t, err, ErrInvalid)
			}
		})
	}
	cases := map[string]func(*Bundle){
		"format": func(b *Bundle) { b.Format = "other" }, "version": func(b *Bundle) { b.Version = 2 },
		"source UID":  func(b *Bundle) { b.Source.ProjectUID = strings.ToLower(bundleTestUID) },
		"source name": func(b *Bundle) { b.Source.ProjectName = " " }, "timestamp": func(b *Bundle) { b.Source.ExportedAt = "yesterday" },
		"entry UID": func(b *Bundle) { b.Workflows[0].UID = "bad" }, "event UID": func(b *Bundle) { b.Workflows[0].DefinitionEventUID = "" },
		"name":               func(b *Bundle) { b.Workflows[0].Name = strings.Repeat("x", 257) },
		"missing definition": func(b *Bundle) { b.Workflows[0].Definition = nil },
		"null definition":    func(b *Bundle) { b.Workflows[0].Definition = []byte("null") },
		"duplicate identity": func(b *Bundle) { b.Workflows = append(b.Workflows, b.Workflows[0]) },
		"unknown definition": func(b *Bundle) { b.Workflows[0].Definition = []byte(`{"version":1,"unknown":1}`) },
		"definition size": func(b *Bundle) {
			b.Workflows[0].Definition = []byte(`{"about":"` + strings.Repeat("x", DefinitionLimit) + `"}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) { b := testBundle(); mutate(&b); require.Error(t, b.Validate()) })
	}
}

func TestBundleHistoricalDefinitionsAndKindScopedIdentity(t *testing.T) {
	b := testBundle()
	b.Workflows[0].Definition = []byte(`{"version":99,"steps":[]}`)
	b.Jobs = append(b.Jobs, BundleDefinition{UID: bundleWorkflowUID, DefinitionEventUID: bundleTestUID, Name: "Historical", Definition: []byte(`{"version":99,"action":{"workflow_uid":"` + bundleWorkflowUID + `"}}`)})
	require.NoError(t, b.Validate(), "archives use structural read validation, not current write validation")
	b.Workflows = nil
	require.ErrorContains(t, b.Validate(), "workflow")
}

func TestBundleCountAndEncodedLimit(t *testing.T) {
	b := testBundle()
	b.Workflows = nil
	for i := range 1000 {
		b.Workflows = append(b.Workflows, BundleDefinition{UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G%04d", i), DefinitionEventUID: bundleTestUID, Name: "Inspect", Definition: []byte(`{"version":1,"steps":[]}`)})
	}
	require.NoError(t, b.Validate())
	b.Jobs = append(b.Jobs, BundleDefinition{UID: bundleWorkflowUID, DefinitionEventUID: bundleTestUID, Name: "Inspect", Definition: []byte(`{"version":99}`)})
	require.ErrorContains(t, b.Validate(), "1000 definitions")
	b.Jobs = nil
	for i := range b.Workflows {
		b.Workflows[i].Definition = []byte(`{"about":"` + strings.Repeat("x", 17000) + `","version":1,"steps":[]}`)
	}
	require.ErrorContains(t, b.Validate(), "size")
	b.Workflows = nil
	require.NoError(t, b.Validate(), "empty export is valid")
}

func TestBundleRequiresCatalogArrays(t *testing.T) {
	raw, err := json.Marshal(testBundle())
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, key := range []string{"jobs", "workflows"} {
		for _, replacement := range []jsontext.Value{nil, []byte("null")} {
			clone := maps.Clone(fields)
			if replacement == nil {
				delete(clone, key)
			} else {
				clone[key] = replacement
			}
			input, err := json.Marshal(clone)
			require.NoError(t, err)
			_, err = ParseBundle(input)
			require.Error(t, err, "%s must be an array", key)
		}
	}
}

func TestBundleInputByteBoundary(t *testing.T) {
	raw, err := json.Marshal(testBundle())
	require.NoError(t, err)
	exact := append(raw, bytes.Repeat([]byte(" "), BundleLimit-len(raw))...)
	_, err = ParseBundle(exact)
	require.NoError(t, err)
	_, err = ParseBundle(append(exact, ' '))
	require.ErrorContains(t, err, "size")
}

func TestBundleTimestampGrammar(t *testing.T) {
	for _, stamp := range []string{"2026-11-04T1:00:00Z", "2026-11-04T00:00:00,5Z", "2026-11-04T00:00:00+24:00", "2026-11-04T00:00:00+01:60", "2026-02-30T00:00:00Z"} {
		b := testBundle()
		b.Source.ExportedAt = stamp
		require.Error(t, b.Validate(), stamp)
	}
	for _, stamp := range []string{"2026-11-04T01:00:00Z", "2024-02-29T00:00:00.123456789-03:30"} {
		b := testBundle()
		b.Source.ExportedAt = stamp
		require.NoError(t, b.Validate(), stamp)
	}
}

func TestBundleOpaqueNumericTokensProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		token := fmt.Sprintf("%d", n)
		spelling := hegel.Draw(ht, hegel.SampledFrom([]string{token, token + ".0", token + "e9999", "-0", "123456789012345678901234567890"}))
		b := testBundle()
		b.Workflows[0].Definition = []byte(`{"version":99,"options":{"n":` + spelling + `},"steps":[{"options":{"n":` + spelling + `}}]}`)
		b.Jobs = []BundleDefinition{{UID: bundleTestUID, DefinitionEventUID: bundleTestUID, Name: "Historical", Definition: []byte(`{"version":99,"options":{"n":` + spelling + `}}`)}}
		raw, err := json.Marshal(b)
		require.NoError(ht, err)
		parsed, err := ParseBundle(raw)
		require.NoError(ht, err)
		require.Equal(ht, b.Workflows[0].Definition, parsed.Workflows[0].Definition)
		require.Equal(ht, b.Jobs[0].Definition, parsed.Jobs[0].Definition)
	}, hegel.WithTestCases(100))
}

func TestBundleValidationDoesNotMutateProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b := testBundle()
		text := hegel.Draw(ht, hegel.Text())
		raw, err := json.Marshal(map[string]any{"version": 99, "about": text})
		require.NoError(ht, err)
		b.Workflows[0].Definition = raw
		before, err := json.Marshal(b)
		require.NoError(ht, err)
		_ = b.Validate()
		after, err := json.Marshal(b)
		require.NoError(ht, err)
		require.Equal(ht, before, after)
		input := bytes.Clone(before)
		_, err = ParseBundle(input)
		require.NoError(ht, err)
		require.Equal(ht, before, input)
	}, hegel.WithTestCases(100))
}

func TestBundleNameByteBoundsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b := testBundle()
		name := hegel.Draw(ht, hegel.Text())
		b.Workflows[0].Name = name
		size := len(strings.TrimSpace(name))
		err := b.Validate()
		if size >= 1 && size <= 256 {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, ErrInvalid)
		}
	}, hegel.WithTestCases(100))
	for _, size := range []int{255, 256, 257} {
		b := testBundle()
		b.Workflows[0].Name = " \t" + strings.Repeat("é", size/2) + strings.Repeat("x", size%2) + "\n"
		if size <= 256 {
			require.NoError(t, b.Validate())
		} else {
			require.Error(t, b.Validate())
		}
	}
}

func TestBundleNestedStructuralStrictness(t *testing.T) {
	for _, document := range []string{
		`{"version":99,"options":{"x":1,"x":2}}`,
		`{"version":99,"action":{"unknown":true}}`,
		`{"version":99,"trigger":{"unknown":true}}`,
		`{"version":99,"issue":{"unknown":true}}`,
	} {
		b := testBundle()
		b.Jobs = []BundleDefinition{{UID: bundleTestUID, DefinitionEventUID: bundleTestUID, Name: "Inspect", Definition: []byte(document)}}
		require.ErrorIs(t, b.Validate(), ErrInvalid, document)
	}
	b := testBundle()
	b.Workflows[0].Definition = []byte(`{"version":99,"steps":[{"unknown":true}]}`)
	require.ErrorIs(t, b.Validate(), ErrInvalid)
}
