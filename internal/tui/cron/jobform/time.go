package jobform

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
)

// RunAtText formats a stored instant in its configured timezone.
func RunAtText(stored, zone string) (display, edit string, err error) {
	edit = stored
	t, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		return "", edit, fmt.Errorf("run at: %w", err)
	}
	loc, err := jobLocation(zone)
	if err != nil {
		return "", edit, err
	}
	t = t.In(loc)
	layout := "2006-01-02 15:04"
	if t.Second() != 0 || t.Nanosecond() != 0 {
		layout += ":05.999999999"
	}
	return t.Format(layout) + " " + loc.String(), edit, nil
}
func jobLocation(zone string) (*time.Location, error) {
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	return loc, nil
}

// ParseRunAt accepts exactly one wall-time candidate, refusing DST gaps/folds.
func ParseRunAt(input, original, zone string) (string, error) {
	if input == original && original != "" {
		return original, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, input); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	loc, err := jobLocation(zone)
	if err != nil {
		return "", err
	}
	wall, err := time.Parse("2006-01-02 15:04:05", input)
	if err != nil {
		wall, err = time.Parse("2006-01-02 15:04", input)
	}
	if err != nil {
		return "", fmt.Errorf("run at: use RFC3339 with an offset or 2006-01-02 15:04[:05]")
	}
	offsets := map[int]bool{}
	end := wall.Add(48 * time.Hour)
	for t := wall.Add(-48 * time.Hour); !t.After(end); {
		local := t.In(loc)
		_, offset := local.Zone()
		offsets[offset] = true
		_, bound := local.ZoneBounds()
		if bound.IsZero() || bound.After(end) {
			break
		}
		t = bound
	}
	var candidates []time.Time
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		if candidate.In(loc).Format("2006-01-02 15:04:05") == wall.Format("2006-01-02 15:04:05") {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("run at: %s does not exist in %s", input, loc)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
	if len(candidates) > 1 {
		return "", fmt.Errorf("run at: %s is ambiguous in %s; add an offset, e.g. %s", input, loc, candidates[len(candidates)-1].In(loc).Format(time.RFC3339))
	}
	return candidates[0].UTC().Format(time.RFC3339Nano), nil
}

// ParseSeconds accepts a whole-second integer or duration within native bounds.
func ParseSeconds(field, text string, signed bool) (int64, error) {
	text = strings.TrimSpace(text)
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		duration, parseErr := time.ParseDuration(text)
		if parseErr != nil {
			if strings.Contains(text, ".") {
				return 0, fmt.Errorf("%s: whole seconds only", field)
			}
			return 0, fmt.Errorf("%s: use a duration or whole seconds within range", field)
		}
		if duration%time.Second != 0 {
			return 0, fmt.Errorf("%s: whole seconds only", field)
		}
		n = int64(duration / time.Second)
	}
	if n > defs.MaxSeconds || n < -defs.MaxSeconds {
		return 0, fmt.Errorf("%s: duration exceeds supported range", field)
	}
	if n < 0 && !signed {
		return 0, fmt.Errorf("%s: negative duration is not allowed", field)
	}
	return n, nil
}

// SecondsText formats a validated whole-second duration.
func SecondsText(seconds int64) string { return (time.Duration(seconds) * time.Second).String() }
