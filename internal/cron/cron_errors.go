package cron

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type cronFieldBounds struct {
	name     string
	min, max int
}

var cronFields = [...]cronFieldBounds{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 6},
}

func invalidCronExpression(message string) error {
	return invalid("invalid cron expression: " + message)
}

func invalidCronDirective() error {
	return invalidCronExpression("TZ= and CRON_TZ= must be followed by a space and a schedule")
}

func unknownTimezone(name string) error {
	return invalid(fmt.Sprintf("timezone %s is not a known IANA name; use one such as America/New_York, or UTC", quoteCronToken(name)))
}

func quoteCronToken(value string) string { return quoteCronFragment(value, 40, 0) }

// Truncate before quoting so invalid UTF-8 and controls cannot reach the output.
// Details have an additional rendered limit: quoting 120 NUL bytes would alone
// exceed the complete error's 400-byte budget.
func quoteCronFragment(value string, byteLimit, renderedLimit int) string {
	end := len(value)
	shortened := end > byteLimit
	if shortened {
		end = byteLimit
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
	}
	for {
		fragment := value[:end]
		if shortened {
			fragment += "…"
		}
		quoted := strconv.Quote(fragment)
		if renderedLimit == 0 || len(quoted) <= renderedLimit {
			return quoted
		}
		_, size := utf8.DecodeLastRuneInString(value[:end])
		end -= size
		shortened = true
	}
}

func explainCron(expression string, parseErr error) error {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return invalidCronExpression("empty; use five fields (minute hour day-of-month month day-of-week) or a descriptor such as @daily")
	}
	if hasCronTimezoneDirective(trimmed) && !hasCronTimezoneDirective(expression) {
		return invalidCronExpression("timezone directive must start the expression; remove leading whitespace before TZ= or CRON_TZ=")
	}
	if hasCronTimezoneDirective(expression) {
		directive, rest, _ := strings.Cut(expression, " ")
		if strings.HasPrefix(parseErr.Error(), "provided bad location ") {
			_, zone, _ := strings.Cut(directive, "=")
			return unknownTimezone(zone)
		}
		expression = strings.TrimSpace(rest)
		if hasCronTimezoneDirective(expression) {
			return invalidCronExpression("use only one TZ= or CRON_TZ= timezone directive before the schedule")
		}
		if expression == "" {
			return invalidCronExpression("empty; use five fields (minute hour day-of-month month day-of-week) or a descriptor such as @daily")
		}
	}
	if strings.HasPrefix(expression, "@") {
		switch {
		case expression == "@reboot":
			return invalidCronExpression("@reboot is not supported; Kata schedules run on the calendar, not when a daemon starts. Use a descriptor such as @daily, a five-field schedule, or an interval trigger")
		case expression == "@every" || strings.HasPrefix(expression, "@every "):
			_, duration, _ := strings.Cut(expression, " ")
			return invalidCronExpression(fmt.Sprintf("@every needs a duration such as 90m or 2h, got %s; native recurring jobs use an interval trigger instead", quoteCronToken(duration)))
		default:
			return invalidCronExpression(fmt.Sprintf("unknown descriptor %s; use @yearly, @annually, @monthly, @weekly, @daily, @midnight, @hourly or @every <duration>", quoteCronToken(expression)))
		}
	}
	fields := strings.Fields(expression)
	switch len(fields) {
	case 5:
		for i, token := range fields {
			probe := []string{"*", "*", "*", "*", "*"}
			probe[i] = token
			if _, err := parseCronSpec(strings.Join(probe, " ")); err != nil {
				return explainCronField(cronFields[i], token, err)
			}
		}
	case 6, 7:
		return invalidCronExpression(fmt.Sprintf("%d fields; Kata uses exactly five (minute hour day-of-month month day-of-week) and has no seconds or year field. Removing an extra field keeps the schedule only if it was 0 seconds or a * year; nonzero seconds or a year restriction have no five-field equivalent", len(fields)))
	default:
		unit := "fields"
		if len(fields) == 1 {
			unit = "field"
		}
		return invalidCronExpression(fmt.Sprintf("%d %s; Kata uses five (minute hour day-of-month month day-of-week) or a descriptor such as @daily", len(fields), unit))
	}
	return invalidCronExpression(fmt.Sprintf("%s: %s", quoteCronToken(expression), quoteCronFragment(parseErr.Error(), 120, 120)))
}

func hasCronTimezoneDirective(expression string) bool {
	return strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=")
}

func explainCronField(field cronFieldBounds, token string, parseErr error) error {
	detail := parseErr.Error()
	upper := strings.ToUpper(token)
	var message string
	switch {
	case field.name == "day of month" && strings.Contains(upper, "L"):
		message = `last-day syntax (L) is not supported, and five-field cron cannot say "last day of the month": 28-31 fires on each of those days that exists`
	case field.name == "day of week" && strings.Contains(upper, "L"):
		message = "last-weekday syntax (L) is not supported and has no five-field equivalent"
	case field.name == "day of month" && strings.Contains(upper, "W"):
		message = "nearest-weekday syntax (W) is not supported and has no five-field equivalent"
	case field.name == "day of week" && strings.Contains(token, "#"):
		message = "nth-weekday syntax (#) is not supported. A day-of-month range plus a weekday, such as 8-14 with 2, fires on either, not only when both match"
	case token == "H" || strings.HasPrefix(token, "H(") || strings.HasPrefix(token, "H/"):
		message = "hashed values (H) are not supported; choose a fixed value"
	case strings.Contains(detail, "above maximum"):
		var value int
		if n, _ := fmt.Sscanf(detail, "end of range (%d)", &value); n == 1 {
			if field.name == "day of week" && value == 7 && field.max == 6 {
				message = "7 is above the maximum 6; use 0 or SUN for Sunday"
			} else {
				message = cronOutside(field, value)
			}
		}
	case strings.Contains(detail, "below minimum"):
		var value int
		if n, _ := fmt.Sscanf(detail, "beginning of range (%d)", &value); n == 1 {
			message = cronOutside(field, value)
		}
	case strings.Contains(detail, "beyond end of range"):
		var start, end int
		if n, _ := fmt.Sscanf(detail, "beginning of range (%d) beyond end of range (%d)", &start, &end); n == 2 {
			if start > field.max {
				message = cronOutside(field, start)
			} else if end < field.min {
				message = cronOutside(field, end)
			} else {
				message = "range start is after its end; ranges do not wrap"
				_, element, _ := strings.Cut(detail, ": ")
				if !strings.Contains(element, "/") && strings.Count(element, "-") == 1 {
					message += ", use " + cronRange(start, field.max) + "," + cronRange(field.min, end)
				}
			}
		}
	case strings.Contains(detail, "step of range should be a positive number"), strings.Contains(detail, "too many slashes"):
		message = "steps must be one positive number, as in */15"
	}
	if message == "" {
		message = fmt.Sprintf("not a number, name, range, list or step (%s)", quoteCronFragment(detail, 120, 120))
	}
	return invalidCronExpression(fmt.Sprintf("%s %s: %s", field.name, quoteCronToken(token), message))
}

func cronOutside(field cronFieldBounds, value int) string {
	message := fmt.Sprintf("%d is outside %d-%d", value, field.min, field.max)
	switch field.name {
	case "day of week":
		message += " (0 is Sunday)"
	case "month":
		message += " (names JAN-DEC also work)"
	}
	return message
}

func cronRange(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}
