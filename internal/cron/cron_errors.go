package cron

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	cronparser "github.com/netresearch/go-cron"
)

// explainCron only describes a rejection; cronParser remains the grammar owner.
func explainCron(expression string, err error) error {
	detail := err.Error()
	reject := func(reason string) error {
		return invalid("invalid cron expression: " + safeCronDetail(reason, 330))
	}
	if len(expression) > cronparser.MaxSpecLength {
		return reject(fmt.Sprintf("complete parser input is %d bytes; limit is %d bytes, including any timezone directive", len(expression), cronparser.MaxSpecLength))
	}
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		directive, rest, _ := strings.Cut(expression, " ")
		if strings.IndexFunc(directive, unicode.IsSpace) >= 0 {
			return reject("timezone directive needs an ASCII space before its schedule; tabs and other whitespace do not separate it")
		}
		_, zone, _ := strings.Cut(directive, "=")
		if len(zone) >= 2 && (zone[0] == '\'' && zone[len(zone)-1] == '\'' || zone[0] == '"' && zone[len(zone)-1] == '"') {
			zone = zone[1 : len(zone)-1]
		}
		if strings.HasPrefix(detail, "unknown time zone ") || strings.HasPrefix(detail, "invalid timezone ") {
			return explainTimezone(zone, err)
		}
		if strings.HasPrefix(detail, "missing fields after timezone") {
			return reject(detail)
		}
		expression = strings.TrimSpace(rest)
	}
	fields := strings.Fields(expression)
	for _, field := range fields {
		upper := strings.ToUpper(field)
		if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") {
			return reject("use one uppercase TZ= or CRON_TZ= directive at the start of the expression, without leading whitespace, followed by a space and schedule")
		}
	}
	if strings.HasPrefix(expression, "@") {
		switch {
		case expression == "@reboot":
			return reject("@reboot is not supported; use a supported descriptor, five-field schedule, or interval trigger")
		case expression == "@every" || strings.HasPrefix(expression, "@every "):
			return reject("@every needs a duration such as 90m or 2h; native recurring jobs use an interval trigger instead; " + detail)
		default:
			return reject(fmt.Sprintf("unsupported descriptor expression %q; supported descriptors, such as @daily, must match exactly", clip(expression, 40)))
		}
	}
	if strings.HasPrefix(strings.TrimSpace(expression), "@") {
		return reject("descriptor must start the expression without leading whitespace")
	}

	switch len(fields) {
	case 0:
		return reject("expression is empty; use five fields or a descriptor such as @daily")
	case 6, 7:
		return reject(fmt.Sprintf("%d fields; Kata uses exactly five (minute hour day-of-month month day-of-week), with no seconds or year field; only a 0 seconds field or wildcard * year can be removed without changing the schedule; nonzero seconds or a restricted year has no five-field equivalent", len(fields)))
	case 5:
		names := [...]string{"minute", "hour", "day of month", "month", "day of week"}
		for i, token := range fields {
			probe := []string{"*", "*", "*", "*", "*"}
			probe[i] = token
			if _, fieldErr := cronParser.Parse(strings.Join(probe, " ")); fieldErr != nil {
				reason := fieldErr.Error()
				if i == 2 || i == 4 {
					reason = daySyntaxReason(i, token)
					if reason == "" {
						reason = fieldErr.Error()
						if strings.HasPrefix(reason, "failed to parse int from") {
							reason = "use numeric values or field-appropriate names; " + reason
						}
						if i == 2 && strings.ContainsAny(strings.ToUpper(token), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
							reason = "day of month takes numbers 1-31"
						}
					}
				}
				if strings.Contains(fieldErr.Error(), "hash option") {
					reason = "choose a fixed value; hashed schedules (H) are not supported"
				}
				return reject(fmt.Sprintf("%s %q: %s", names[i], clip(token, 40), reason))
			}
		}
		return reject(detail)
	default:
		unit := "fields"
		if len(fields) == 1 {
			unit = "field"
		}
		return reject(fmt.Sprintf("%d %s; Kata uses exactly five (minute hour day-of-month month day-of-week)", len(fields), unit))
	}
}

func explainTimezone(name string, err error) error {
	label := "invalid timezone"
	if err != nil && strings.HasPrefix(err.Error(), "unknown time zone ") && !strings.Contains(err.Error(), "time: invalid location name") {
		label = "unknown time zone"
	}
	return invalid(fmt.Sprintf("%s %q; use an IANA name such as America/New_York, or UTC", label, clip(name, 40)))
}

var (
	lastDayToken    = regexp.MustCompile(`(?i)^L(-[0-9]+)?$`)
	nearestDayToken = regexp.MustCompile(`(?i)^[0-9]+W$`)
	weekdayToken    = regexp.MustCompile(`(?i)^([0-9]+|SUN|MON|TUE|WED|THU|FRI|SAT)(L|#(L|[0-9]+))$`)
)

// Recognize unsupported constructs, including list components, solely for advice.
func daySyntaxReason(field int, token string) string {
	for _, part := range strings.Split(strings.ToUpper(token), ",") {
		switch {
		case field == 2 && part == "LW", field == 4 && weekdayToken.MatchString(part) && strings.HasSuffix(part, "L"):
			return "last-weekday syntax is not supported and has no five-field equivalent"
		case field == 2 && lastDayToken.MatchString(part):
			return "last-day syntax (L) is not supported and has no five-field equivalent"
		case field == 2 && nearestDayToken.MatchString(part):
			return "nearest-weekday syntax (W) is not supported and has no five-field equivalent"
		case field == 4 && strings.Contains(part, "#"):
			return "nth-weekday syntax (#) is not supported and has no five-field equivalent"
		}
	}
	return ""
}

// clip bounds raw values before quoting, keeping complete UTF-8 runes.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n - len("…")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// Library errors already quote values. Escape raw controls, replace invalid
// UTF-8, and bound rendered bytes without double-quoting library detail.
func safeCronDetail(s string, n int) string {
	var out strings.Builder
	for _, r := range s {
		part := string(r)
		if r < 0x20 || r == 0x7f {
			quoted := strconv.QuoteRune(r)
			part = quoted[1 : len(quoted)-1]
		}
		if out.Len()+len(part) > n-len("…") {
			return out.String() + "…"
		}
		out.WriteString(part)
	}
	return out.String()
}
