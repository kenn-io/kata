package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

var cronPresets = []string{"0 * * * *", "0 9 * * *", "0 9 * * 1-5", "0 9 * * 1", "0 17 * * 5"}

func cronPresetValue(raw string) string {
	if normalized := strings.Join(strings.Fields(raw), " "); slices.Contains(cronPresets, normalized) {
		return normalized
	}
	return raw
}

func (f *cronForm) presetChoices() []cronChoice {
	out := make([]cronChoice, 0, len(cronPresets)+1)
	for _, expr := range cronPresets {
		label, _ := cronScheduleLabel(expr)
		out = append(out, cronChoice{expr, label})
	}
	for _, raw := range []string{f.val[cfCron], f.val[cfPreset], f.snapshot[cfCron]} {
		if strings.TrimSpace(raw) != "" && !slices.Contains(cronPresets, cronPresetValue(raw)) {
			return append(out, cronChoice{raw, "Custom"})
		}
	}
	return out
}

// cronScheduleLabel names only simple, unambiguous schedules. The shared parser
// remains authoritative; unfamiliar syntax stays visible as the raw expression.
func cronScheduleLabel(expr string) (string, bool) {
	if _, err := cron.ParseCron(expr); err != nil {
		return "", false
	}
	switch strings.TrimSpace(expr) {
	case "@hourly":
		return "Hourly", true
	case "@daily", "@midnight":
		return "Daily 00:00", true
	case "@weekly":
		return "Sundays 00:00", true
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 || fields[2] != "*" || fields[3] != "*" {
		return "", false
	}
	minute, ok := cronDecimal(fields[0], 59)
	if !ok {
		return "", false
	}
	if fields[1] == "*" && fields[4] == "*" {
		if minute == 0 {
			return "Hourly", true
		}
		return fmt.Sprintf("Hourly at :%02d", minute), true
	}
	hour, ok := cronDecimal(fields[1], 23)
	if !ok {
		return "", false
	}
	day := fields[4]
	label := "Daily"
	switch {
	case day == "*":
	case day == "1-5":
		label = "Weekdays"
	case len(day) == 1 && day[0] >= '0' && day[0] <= '7':
		label = [...]string{"Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays", "Sundays"}[day[0]-'0']
	default:
		return "", false
	}
	return fmt.Sprintf("%s %02d:%02d", label, hour, minute), true
}

func cronDecimal(text string, maxValue int) (int, bool) {
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil && n <= maxValue
}

func cronScheduleDetail(t generated.TriggerResponse) string {
	if t.Kind == "cron" {
		if label, ok := cronScheduleLabel(deref(t.Cron)); ok {
			raw := deref(t.Cron)
			if zone := deref(t.Timezone); zone != "" && zone != "UTC" {
				raw += " (" + zone + ")"
			}
			return label + " · " + raw
		}
	}
	return cronSchedule(t)
}
