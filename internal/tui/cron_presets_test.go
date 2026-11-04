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
		label, ok := cronScheduleLabel(expr)
		require.True(t, ok)
		require.Equal(t, labels[i], label)
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
	for _, raw := range []string{"", "  ", "bad schedule"} {
		f := newNativeForm(t)
		f.val[cfCron] = raw
		f.step(cfPreset, 1)
		require.Equal(t, "0 * * * *", f.val[cfCron])
	}
}

func TestCronPresetRoundTrips(t *testing.T) {
	expressions := presetCronExpressionGen()
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{cronRecurringID, "prompt", "workflow", "notify"}))
		opened := hegel.Draw(ht, expressions)
		typed := hegel.Draw(ht, expressions)
		zone := hegel.Draw(ht, hegel.SampledFrom([]string{"UTC", "Europe/Paris", "America/New_York"}))
		f := newNativeForm(t)
		f.kind = kind
		f.val[cfPrompt], f.val[cfWorkflow], f.val[cfIssue] = "Example prompt", cronTestIssue, cronTestIssue
		f.val[cfCron], f.val[cfZone] = opened, zone
		_, def, err := f.build(cronTestUIDs)
		require.NoError(ht, err)
		rec := nativeRecord()
		rec.Definition = def
		f, err = cronFormFor(rec, nil)
		require.NoError(ht, err)
		require.False(ht, f.dirty())
		_, def, err = f.build(cronTestUIDs)
		require.NoError(ht, err)
		require.Equal(ht, opened, def.Trigger.Cron)

		f.val[cfCron] = typed
		options := slices.Clone(presetTestExpressions)
		for _, candidate := range []string{typed, opened} {
			if !slices.Contains(presetTestExpressions, strings.Join(strings.Fields(candidate), " ")) {
				options = append(options, candidate)
				break
			}
		}
		current := typed
		walk := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]int{-1, 1})).MaxSize(30))
		// Include a complete cycle after any walk, in either direction.
		delta := hegel.Draw(ht, hegel.SampledFrom([]int{-1, 1}))
		walk = append(walk, slices.Repeat([]int{delta}, len(options))...)
		for _, step := range walk {
			identity := strings.Join(strings.Fields(current), " ")
			at := slices.Index(presetTestExpressions, identity)
			if at < 0 {
				at = len(options) - 1
			}
			current = options[(at+step+len(options))%len(options)]
			f.step(cfPreset, step)
			_, def, err = f.build(cronTestUIDs)
			require.NoError(ht, err)
			require.Equal(ht, current, def.Trigger.Cron)
			require.Equal(ht, zone, def.Trigger.Timezone)
		}
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

func TestCronPresetArbitraryCustomDrafts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.OneOf(hegel.Text(), presetCronExpressionGen()))
		delta := hegel.Draw(ht, hegel.SampledFrom([]int{-1, 1}))
		f := newNativeForm(t)
		f.val[cfCron] = raw
		f.focusField(cfPreset)
		identity := strings.Join(strings.Fields(raw), " ")
		want := raw
		steps := 6
		if slices.Contains(presetTestExpressions, identity) {
			want, steps = identity, 5
		}
		if strings.TrimSpace(raw) == "" {
			// An empty field can enter the presets, but is not a Custom choice.
			f.step(cfPreset, delta)
			if delta == 1 {
				require.Equal(ht, "0 * * * *", f.val[cfCron])
			} else {
				require.Equal(ht, "0 17 * * 5", f.val[cfCron])
			}
			return
		}
		for range steps {
			f.step(cfPreset, delta)
		}
		require.Equal(ht, want, f.val[cfCron])
	}, hegel.WithTestCases(1000))
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
	for _, paste := range []bool{false, true} {
		m, _ := cronWriteFixture(t)
		m = sendRune(m, 'n')
		f := m.cron.form
		f.val[cfCron] = ""
		f.focusField(cfCron)
		if paste {
			m, _ = updateModel(m, tea.PasteMsg{Content: "0 17 * * 5"})
		} else {
			for _, r := range "0 17 * * 5" {
				m = sendRune(m, r)
			}
		}
		require.Contains(t, stripANSI(m.viewBody()), "‹ Fridays 17:00 ›")
		f.val[cfCron] = ""
		f.focusField(cfCron)
		m, _ = updateModel(m, tea.PasteMsg{Content: "30 8 * * 1-5"})
		f.focusField(cfPreset)
		for _, key := range []rune{tea.KeyRight, tea.KeyLeft, tea.KeySpace, tea.KeyLeft} {
			m = sendKey(m, key)
		}
		require.Equal(t, "30 8 * * 1-5", f.val[cfCron])
		f.step(cfTrigger, 1)
		require.False(t, f.shown(cfPreset))
		f.step(cfTrigger, -1)
		require.Equal(t, "Custom", f.display(cfPreset))
		f.step(cfKind, 1)
		f.step(cfKind, -1)
		for range 6 {
			f.step(cfPreset, 1)
		}
		require.Equal(t, "30 8 * * 1-5", f.val[cfCron])
	}
}

func TestCronScheduleFilterUsesLabels(t *testing.T) {
	for _, tc := range []struct {
		expr, query string
		count       int
	}{
		{"0 9 * * 1-5", "weekdays", 1},
		{"0 9 * * 1-5", "1-5", 0},
		{"*/7 * * * *", "*/7", 1},
	} {
		m := cronFixture(t)
		m.cron.data.jobs[0].Name = "Example job"
		m.cron.data.jobs[0].Definition.Trigger.Cron = new(tc.expr)
		m = sendRune(m, '/')
		m, _ = updateModel(m, tea.PasteMsg{Content: tc.query})
		m = sendKey(m, tea.KeyEnter)
		_, _, rows := cronScreen(m)
		require.Len(t, rows, tc.count)
	}
}

func TestSnapshot_CronNewForm(t *testing.T) {
	defer snapshotInit(t)()
	m, _ := cronWriteFixture(t)
	m = sendRune(m, 'n')
	m.cron.form.uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	require.Contains(t, stripANSI(m.viewBody()), "Preset")
	assertGolden(t, "cron-form-new", m.viewBody())
}
