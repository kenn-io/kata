package tui

import (
	"encoding/json/v2"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/metadata"
)

// dateInputHint names the forms the Dates form reads, for the message that
// refuses anything else.
const dateInputHint = "YYYY-MM-DD, YYYY-MM-DDTHH:MM[:SS], YYYY-MM-DDTHH:MM:SSZ, today, tomorrow, +Nd or +Nw"

var relativeDate = regexp.MustCompile(`^\+(\d{1,4})([dw])$`)

// resolveDateInput reads one Dates form field. Blank input returns "", which
// means no date. today, tomorrow, +Nd and +Nw count from the calendar day of
// now in now's own zone and resolve to YYYY-MM-DD; they exist only in the TUI,
// so only the resolved day ever reaches the daemon. Every other text must
// already be a value the daemon stores for scheduled_on and deadline_on, the
// same set kata schedule and kata deadline accept. The error does not name a
// field; the caller adds the label.
func resolveDateInput(input string, now time.Time) (string, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return "", nil
	}
	lower := strings.ToLower(text)
	var days int
	switch match := relativeDate.FindStringSubmatch(lower); {
	case lower == "today":
	case lower == "tomorrow":
		days = 1
	case match != nil:
		days, _ = strconv.Atoi(match[1])
		if match[2] == "w" {
			days *= 7
		}
	default:
		if !validStoredDate(text) {
			return "", errors.New("must be " + dateInputHint + ".")
		}
		return text, nil
	}
	year, month, day := now.Date()
	return time.Date(year, month, day+days, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), nil
}

// validStoredDate asks the daemon's own validator, so the form never accepts
// a value the daemon would reject.
func validStoredDate(text string) bool {
	encoded, err := json.Marshal(text)
	if err != nil {
		return false
	}
	return metadata.Validate(metadata.IssueRegistry, api.MetadataKeyScheduledOn, encoded) == nil
}

// datePreview is the muted line under a date field: the day the text resolves
// to with its weekday ("2026-11-05 Thu"), "no date" for blank, and nothing for
// text that saving would refuse. A local time keeps its clock time; a UTC
// instant is shown as typed because its calendar day depends on the zone it
// is read in.
func datePreview(input string, now time.Time) string {
	value, err := resolveDateInput(input, now)
	switch {
	case err != nil:
		return ""
	case value == "":
		return "no date"
	case strings.HasSuffix(value, "Z"):
		return value
	}
	day, err := time.Parse(time.DateOnly, value[:len(time.DateOnly)])
	if err != nil {
		return ""
	}
	preview := value[:len(time.DateOnly)] + " " + day.Format("Mon")
	if clock := value[len(time.DateOnly):]; strings.HasPrefix(clock, "T") {
		preview += " " + clock[1:]
	}
	return preview
}
