package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	_ "time/tzdata"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/cron"
)

// Literal expectations describe the user's choices, independent of the table
// that production uses to fill Cron.
var presetTestExpressions = []string{"0 * * * *", "0 9 * * *", "0 9 * * 1-5", "0 9 * * 1", "0 17 * * 5"}

func TestCronPresetFieldAndValues(t *testing.T) {
	labels := []string{"Hourly", "Daily 09:00", "Weekdays 09:00", "Mondays 09:00", "Fridays 17:00"}
	for _, kind := range cronJobKinds {
		f := newNativeForm(t)
		f.kind = kind.id
		for _, trigger := range kind.triggers {
			f.trigger = trigger
			require.Equal(t, trigger == "cron", slices.Contains(f.order(), cfPreset), "%s/%s", kind.id, trigger)
		}
	}
	f := newNativeForm(t)
	order := f.order()
	at := slices.Index(order, cfPreset)
	require.Positive(t, at)
	require.Equal(t, []cronField{cfEnabled, cfPreset, cfCron, cfZone}, order[at-1:at+3])
	f.val[cfZone] = "Europe/Paris"
	f.val[cfCron] = presetTestExpressions[0]
	for i, expr := range presetTestExpressions {
		require.Equal(t, labels[i], f.display(cfPreset))
		_, def, err := f.build(cronTestUIDs)
		require.NoError(t, err)
		require.Equal(t, expr, def.Trigger.Cron)
		require.Equal(t, "Europe/Paris", def.Trigger.Timezone)
		f.step(cfPreset, 1)
	}
	require.Equal(t, "Hourly", f.display(cfPreset))
}

func TestCronPresetRetainsCustomAndPristineState(t *testing.T) {
	for _, opened := range []string{"0 9 * * *", "15 6 * * *", "0 9 * * 1\t"} {
		for _, delta := range []int{-1, 1} {
			rec := nativeRecord()
			rec.Definition.Trigger.Cron = opened
			f, err := cronFormFor(rec, nil)
			require.NoError(t, err)
			f.focusField(cfPreset)
			_ = f.display(cfPreset)
			require.False(t, f.dirty())
			_, def, err := f.build(cronTestUIDs)
			require.NoError(t, err)
			require.Equal(t, opened, def.Trigger.Cron)

			custom := "30\t8 * * 1-5 "
			f.val[cfCron] = custom
			require.Equal(t, "Custom", f.display(cfPreset))
			for range 6 {
				f.step(cfPreset, delta)
			}
			require.Equal(t, custom, f.val[cfCron], "latest typed custom must survive a full cycle")
			f.val[cfCron] = opened
			require.False(t, f.dirty(), "remembering Custom alone is not an edit")
		}
	}
}

func TestCronPresetArbitraryCustomDrafts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.OneOf(hegel.Text(), presetCronExpressionGen()))
		zone := hegel.Draw(ht, hegel.SampledFrom([]string{"UTC", "Europe/Paris", "America/New_York"}))
		f := newNativeForm(t)
		f.val[cfCron], f.val[cfZone] = raw, zone
		f.focusField(cfPreset)
		options := slices.Clone(presetTestExpressions)
		identity := strings.Join(strings.Fields(raw), " ")
		if strings.TrimSpace(raw) != "" && !slices.Contains(options, identity) {
			options = append(options, raw)
		}
		current := raw
		step := func(delta int) {
			at := slices.Index(options, strings.Join(strings.Fields(current), " "))
			if at < 0 {
				at = slices.Index(options, current)
			}
			if at < 0 && delta < 0 {
				at = 0
			}
			current = options[(at+delta+len(options))%len(options)]
			f.step(cfPreset, delta)
			require.Equal(ht, current, f.val[cfCron])
			require.Equal(ht, zone, f.val[cfZone])
			_, def, err := f.build(cronTestUIDs)
			if _, parseErr := cron.ParseCron(current); parseErr != nil {
				require.Error(ht, err)
			} else {
				require.NoError(ht, err)
				require.Equal(ht, current, def.Trigger.Cron)
				require.Equal(ht, zone, def.Trigger.Timezone)
			}
		}
		walk := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]int{-1, 1})).MaxSize(30))
		for _, delta := range walk {
			step(delta)
		}
		// A blank draft enters the presets, but is not remembered as Custom.
		delta := hegel.Draw(ht, hegel.SampledFrom([]int{-1, 1}))
		if strings.TrimSpace(current) == "" {
			step(delta)
		}
		beforeCycle := current
		if normalized := strings.Join(strings.Fields(current), " "); slices.Contains(presetTestExpressions, normalized) {
			beforeCycle = normalized
		}
		for range len(options) {
			step(delta)
		}
		require.Equal(ht, beforeCycle, f.val[cfCron])
	}, hegel.WithTestCases(1000))
}

func presetCronExpressionGen() hegel.Generator[string] {
	grammar := hegel.Composite(func(tc hegel.TestCase) string {
		minute := hegel.Draw(tc, hegel.Integers(0, 59))
		hour := hegel.Draw(tc, hegel.Integers(0, 23))
		day := hegel.Draw(tc, hegel.SampledFrom([]string{"*", "1-5", "0", "1", "2", "3", "4", "5", "6", "7"}))
		return fmt.Sprintf("%02d %02d * * %s\t", minute, hour, day)
	})
	corpus := append(slices.Clone(presetTestExpressions), "30 8 * * 1-5", "*/7 * * * *", "0 9 15 * 1", "@weekly", "@monthly", "0 9 * * 1\t")
	return hegel.OneOf(grammar, hegel.SampledFrom(corpus))
}

func TestCronPresetHiddenDrafts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{cronRecurringID, "prompt", "workflow", "notify"}))
		raw := hegel.Draw(ht, presetCronExpressionGen())
		f := newNativeForm(t)
		f.kind = kind
		f.val[cfCron], f.val[cfZone] = raw, "Europe/Paris"
		f.focusField(cfPreset)
		f.step(cfPreset, 1)
		selected := f.val[cfCron]
		for range len(f.kindDef().triggers) {
			f.step(cfTrigger, 1)
		}
		for range len(cronJobKinds) {
			f.step(cfKind, 1)
		}
		require.Equal(ht, kind, f.kind)
		require.Equal(ht, "cron", f.trigger)
		require.Equal(ht, selected, f.val[cfCron])
		require.Equal(ht, "Europe/Paris", f.val[cfZone])
		f.step(cfPreset, -1)
		want := raw
		if normalized := strings.Join(strings.Fields(raw), " "); slices.Contains(presetTestExpressions, normalized) {
			want = normalized
		}
		require.Equal(ht, want, f.val[cfCron])
	}, hegel.WithTestCases(1000))
}

func TestCronPresetFollowsTypedAndPastedCron(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m = sendRune(m, 'n')
	f := m.cron.form
	f.val[cfCron] = ""
	f.focusField(cfCron)
	for _, r := range "0 17 * * 5" {
		m = sendRune(m, r)
	}
	require.Contains(t, stripANSI(m.viewBody()), "‹ Fridays 17:00 ›")
	f.val[cfCron] = ""
	f.focusField(cfCron)
	m, _ = updateModel(m, tea.PasteMsg{Content: "30 8 * * 1-5"})
	f.focusField(cfPreset)
	for _, selection := range []struct {
		key   rune
		expr  string
		label string
	}{
		{tea.KeyRight, "0 * * * *", "Hourly"},
		{tea.KeyLeft, "30 8 * * 1-5", "Custom"},
		{tea.KeySpace, "0 * * * *", "Hourly"},
		{tea.KeyLeft, "30 8 * * 1-5", "Custom"},
	} {
		m = sendKey(m, selection.key)
		require.Equal(t, selection.expr, f.val[cfCron], "key=%d", selection.key)
		require.Contains(t, stripANSI(m.viewBody()), "‹ "+selection.label+" ›")
	}
}

func TestCronScheduleFilterUsesLabels(t *testing.T) {
	m := cronFixture(t)
	m.cron.data.jobs[0].Name = "Example job"
	m.cron.data.jobs[0].Definition.Trigger.Cron = new("0 9 * * 1-5")
	m = sendRune(m, '/')
	m, _ = updateModel(m, tea.PasteMsg{Content: "weekdays"})
	m = sendKey(m, tea.KeyEnter)
	_, _, rows := cronScreen(m)
	require.Len(t, rows, 1)
}

func TestSnapshot_CronNewForm(t *testing.T) {
	defer snapshotInit(t)()
	m, _ := cronWriteFixture(t)
	m = sendRune(m, 'n')
	m.cron.form.uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	assertGolden(t, "cron-form-new", m.viewBody())
}
