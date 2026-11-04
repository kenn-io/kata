package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

func choiceForm(t *testing.T, raw string) *cronForm {
	t.Helper()
	f, err := cronFormFor(executeRecord(raw), nil)
	require.NoError(t, err)
	f.setRuntimeInventory(&cron.RuntimeInventory{Runtimes: []cron.Runtime{{Label: "worker", Adapter: "example", Choices: map[string][]string{"options.agent": {"default", "review", "(unset)", "other…"}, "options.model": {"model-a"}, "checkout_key": {"example-project"}}, Status: "live"}}})
	return f
}
func TestCronChoiceRawOptionsProperty(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		value := hegel.Draw(h, hegel.Text())
		key := hegel.Draw(h, hegel.SampledFrom([]string{"agent", "model"}))
		raw := ` { "future":{"keep":[1,2]}, "big":12345678901234567890123, "exact":1.10 } `
		got, err := cronSetRuntimeOption(raw, key, value)
		if err != nil {
			h.Fatal(err)
		}
		var fields map[string]jsontext.Value
		if json.Unmarshal([]byte(got), &fields) != nil {
			h.Fatal("invalid object")
		}
		var read string
		if value == "" {
			if _, ok := fields[key]; ok {
				h.Fatal("unset retained")
			}
		} else if json.Unmarshal(fields[key], &read) != nil || read != value {
			h.Fatal("write/read mismatch")
		}
		if string(fields["big"]) != "12345678901234567890123" || string(fields["exact"]) != "1.10" || string(fields["future"]) != `{"keep":[1,2]}` {
			h.Fatal("unrelated values changed")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
func TestCronRuntimeFormUntouchedAndAuthoritative(t *testing.T) {
	raw := ` {"agent":"gone","big":12345678901234567890123,"exact":1.10} `
	f := choiceForm(t, raw)
	require.True(t, f.isChoice(cfAgent))
	require.Contains(t, f.display(cfAgent), "not advertised")
	for range 40 {
		f.move(1)
		f.validate()
	}
	require.False(t, f.dirty())
	_, def, err := f.build(func(s string) string { return s })
	require.NoError(t, err)
	require.Equal(t, raw, string(def.Options))
	f.val[cfOptions] = `{"agent":42,"model":"model-a","future":[1,2]}`
	f.validate()
	require.False(t, f.shown(cfAgent))
	require.True(t, f.shown(cfModel))
	f.val[cfOptions] = `{"executor":"elsewhere","agent":"review"}`
	f.validate()
	require.False(t, f.shown(cfAgent))
	require.False(t, f.isChoice(cfCheckout))
	f.val[cfOptions] = `[]`
	f.validate()
	require.False(t, f.shown(cfAgent))
	require.False(t, f.isChoice(cfCheckout))
}
func TestCronRuntimeFormActionsAndFreeText(t *testing.T) {
	f := choiceForm(t, `{"agent":"review","big":12345678901234567890123}`)
	f.focusField(cfAgent)
	// Explicit action identity keeps literal sentinel-looking advertisements usable.
	for _, literal := range []string{"(unset)", "other…"} {
		actions := f.runtimeActions(cfAgent)
		idx := slices.IndexFunc(actions, func(a cronRuntimeAction) bool { return a.kind == cronRuntimeValue && a.value == literal })
		require.GreaterOrEqual(t, idx, 0)
		f.applyRuntimeAction(cfAgent, actions[idx])
		require.Equal(t, literal, f.current(cfAgent))
		require.True(t, f.isChoice(cfAgent))
	}
	before := f.val[cfOptions]
	f.applyRuntimeAction(cfAgent, cronRuntimeAction{kind: cronRuntimeOther})
	require.Equal(t, before, f.val[cfOptions])
	require.False(t, f.isChoice(cfAgent))
	f.setRuntimeInventory(nil)
	require.True(t, f.shown(cfAgent))
	require.Equal(t, "other…", f.ed.activeField().value())
	f.ed.activeField().setValue("custom")
	f.validate()
	require.Equal(t, "custom", f.current(cfAgent))
	_, def, err := f.build(func(s string) string { return s })
	require.NoError(t, err)
	require.Contains(t, string(def.Options), `"agent":"custom"`)
	require.Contains(t, string(def.Options), "12345678901234567890123")
	f.applyRuntimeAction(cfAgent, cronRuntimeAction{kind: cronRuntimeUnset})
	require.Equal(t, "", f.current(cfAgent))
}
func TestCronRuntimeFormGoldens(t *testing.T) {
	defer snapshotInit(t)()
	for _, name := range []string{"agent_picker", "checkout_other_text", "not_advertised_label", "no_choices_unchanged"} {
		t.Run(name, func(t *testing.T) {
			m := cronFixture(t)
			m.height = 34
			f := choiceForm(t, `{"agent":"review"}`)
			switch name {
			case "checkout_other_text":
				f.applyRuntimeAction(cfCheckout, cronRuntimeAction{kind: cronRuntimeOther})
			case "not_advertised_label":
				f.val[cfOptions] = `{"agent":"missing"}`
				f.validate()
				f.focusField(cfAgent)
			case "no_choices_unchanged":
				f.setRuntimeInventory(nil)
			default:
				f.focusField(cfAgent)
			}
			m.cron.form = f
			assertGolden(t, fmt.Sprintf("cron_form_%s", name), renderCronForm(m))
		})
	}
}

// Contract: refreshing suggestions and moving focus never edits raw options.
func TestCronRuntimeFormUntouchedProperty(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		value := hegel.Draw(h, hegel.Text())
		encoded, err := json.Marshal(value)
		if err != nil {
			h.Fatal(err)
		}
		raw := ` { "agent": ` + string(encoded) + `, "big":12345678901234567890123, "exact":1.10 } `
		f, err := cronFormFor(executeRecord(raw), nil)
		if err != nil {
			h.Fatal(err)
		}
		advertised := hegel.Draw(h, hegel.Booleans())
		inv := (*cron.RuntimeInventory)(nil)
		if advertised {
			inv = &cron.RuntimeInventory{Runtimes: []cron.Runtime{{Choices: map[string][]string{"options.agent": {"default"}}, Status: "live"}}}
		}
		f.setRuntimeInventory(inv)
		for range hegel.Draw(h, hegel.Integers(0, 80)) {
			f.move(1)
			f.validate()
		}
		_, def, err := f.build(func(s string) string { return s })
		if err != nil || string(def.Options) != raw || f.dirty() {
			h.Fatalf("refresh/navigation edited untouched options: %q err=%v", def.Options, err)
		}
	}, hegel.WithTestCases(500), hegel.WithDatabase(t.TempDir()))
}

func TestCronRuntimeFormFreeTextUnfocused(t *testing.T) {
	for _, field := range []cronField{cfAgent, cfModel} {
		f := choiceForm(t, `{}`)
		f.focusField(field)
		f.applyRuntimeAction(field, cronRuntimeAction{kind: cronRuntimeOther})
		f.ed.activeField().setValue("custom")
		f.validate()
		f.focusField(cfName)
		m := cronFixture(t)
		lines, _ := cronFormLines(m, f)
		found := false
		for _, line := range lines {
			if strings.Contains(line, cronFieldLabels[field]+":") {
				require.Contains(t, line, "custom")
				found = true
			}
		}
		require.True(t, found)
	}
}

func TestCronRuntimeUnsetRemovesEmptyMember(t *testing.T) {
	for _, key := range []string{"agent", "model"} {
		raw := `{"` + key + `":"","future":1.10}`
		got, err := cronSetRuntimeOption(raw, key, "")
		require.NoError(t, err)
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(got), &fields))
		require.NotContains(t, fields, key)
		require.Equal(t, "1.10", string(fields["future"]))
	}
}

func TestCronRuntimeEmptySelectionIsPresent(t *testing.T) {
	f := choiceForm(t, ` {"agent":"","future":1.10} `)
	require.Equal(t, `"" (not advertised)`, f.display(cfAgent))
	actions := f.runtimeActions(cfAgent)
	found := slices.ContainsFunc(actions, func(a cronRuntimeAction) bool { return a.kind == cronRuntimeValue && a.value == "" })
	require.True(t, found)
	before := f.val[cfOptions]
	f.focusField(cfAgent)
	f.applyRuntimeAction(cfAgent, cronRuntimeAction{kind: cronRuntimeOther})
	f.setRuntimeInventory(nil)
	f.validate()
	require.Equal(t, before, f.val[cfOptions])
	require.False(t, f.dirty())
	f.applyRuntimeAction(cfAgent, cronRuntimeAction{kind: cronRuntimeUnset})
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(f.val[cfOptions]), &fields))
	require.NotContains(t, fields, "agent")
}

// Contract: display cost must not decode Options once per advertised candidate.
func TestCronRuntimeDisplayAllocationBound(t *testing.T) {
	f := choiceForm(t, `{"agent":"not-advertised","future":"`+strings.Repeat("x", 128*1024)+`"}`)
	counts := []float64{}
	for _, n := range []int{1, 8192} {
		values := make([]string, n)
		for i := range n {
			values[i] = fmt.Sprintf("agent-%05d", i)
		}
		f.runtimeChoices["options.agent"] = values
		counts = append(counts, testing.AllocsPerRun(1, func() { _ = f.display(cfAgent) }))
	}
	require.LessOrEqual(t, counts[1], counts[0]+10, "display allocations grow with candidate count")
}

func TestCronRuntimeChoicePresenceProperty(t *testing.T) {
	for _, property := range []string{"selection", "free_text", "unset"} {
		t.Run(property, func(t *testing.T) {
			hegel.Test(t, func(h *hegel.T) {
				key := hegel.Draw(h, hegel.SampledFrom([]string{"agent", "model"}))
				value := hegel.Draw(h, hegel.Text())
				present := hegel.Draw(h, hegel.Booleans())
				encoded, err := json.Marshal(value)
				if err != nil {
					h.Fatal(err)
				}
				raw := ` { "future":1.10`
				if present {
					raw += `, "` + key + `":` + string(encoded)
				}
				raw += ` } `
				f := choiceForm(t, raw)
				field := cfAgent
				if key == "model" {
					field = cfModel
				}
				f.focusField(field)
				switch property {
				case "selection":
					actual, exists := f.runtimeSelection(field)
					if exists != present || present && actual != value {
						h.Fatal("presence or exact value lost")
					}
				case "free_text":
					f.applyRuntimeAction(field, cronRuntimeAction{kind: cronRuntimeOther})
					f.setRuntimeInventory(nil)
					f.validate()
					if f.val[cfOptions] != raw || f.dirty() {
						h.Fatal("free-text mode rewrote an untouched option")
					}
				case "unset":
					f.applyRuntimeAction(field, cronRuntimeAction{kind: cronRuntimeUnset})
					var fields map[string]jsontext.Value
					if json.Unmarshal([]byte(f.val[cfOptions]), &fields) != nil {
						h.Fatal("invalid options")
					}
					if _, ok := fields[key]; ok {
						h.Fatal("unset retained a member")
					}
					if string(fields["future"]) != "1.10" {
						h.Fatal("unset changed an unrelated member")
					}
				}
			}, hegel.WithTestCases(500), hegel.WithDatabase(t.TempDir()))
		})
	}
}

// Editing the same member produces stable visible and persisted Options text.
func TestCronRuntimeOptionsDeterministic(t *testing.T) {
	for _, value := range []string{"review", ""} {
		raw := `{"z":1.10,"agent":"old","big":12345678901234567890123,"nested":{"keep":[1,2]}}`
		want := `{"agent":"review","big":12345678901234567890123,"nested":{"keep":[1,2]},"z":1.10}`
		if value == "" {
			want = `{"big":12345678901234567890123,"nested":{"keep":[1,2]},"z":1.10}`
		}
		for range 20 {
			got, err := cronSetRuntimeOption(raw, "agent", value)
			require.NoError(t, err)
			require.Equal(t, want, got)
		}
	}
}
