package cron

import (
	"fmt"
	"strconv"
	"strings"
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
		return reject(detail)
	}
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		zone, rest, _, _ := splitCronDirective(expression)
		if strings.HasPrefix(detail, "unknown time zone ") {
			if len(zone) >= 2 && (zone[0] == '\'' && zone[len(zone)-1] == '\'' || zone[0] == '"' && zone[len(zone)-1] == '"') {
				zone = zone[1 : len(zone)-1]
			}
			return unknownTimezone(zone)
		}
		if strings.HasPrefix(detail, "invalid timezone ") || strings.HasPrefix(detail, "missing fields after timezone") {
			return reject(detail)
		}
		expression = rest
	}
	if strings.HasPrefix(expression, "@") {
		switch {
		case expression == "@reboot":
			return reject("@reboot is not supported; use a supported descriptor, five-field schedule, or interval trigger")
		case expression == "@every" || strings.HasPrefix(expression, "@every "):
			return reject("@every needs a duration such as 90m or 2h; native recurring jobs use an interval trigger instead; " + safeCronDetail(detail, 160))
		default:
			return reject(fmt.Sprintf("unknown descriptor %q; use a descriptor such as @daily", clip(expression, 40)))
		}
	}
	if strings.HasPrefix(strings.TrimSpace(expression), "@") {
		return reject("descriptor must start the expression without leading whitespace")
	}
	fields := strings.Fields(expression)
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
				reason := safeCronDetail(fieldErr.Error(), 160)
				upper := strings.ToUpper(token)
				switch {
				case i == 2 && strings.Contains(upper, "L"):
					reason = "last-day syntax (L) is not supported and has no five-field equivalent; 28-31 fires on each of those days that exists"
				case i == 2 && strings.Contains(upper, "W"):
					reason = "nearest-weekday syntax (W) is not supported and has no five-field equivalent"
				case i == 4 && strings.Contains(token, "#"):
					reason = "nth-weekday syntax (#) is not supported and has no five-field equivalent"
				case i == 4 && strings.Contains(upper, "L"):
					reason = "last-weekday syntax (L) is not supported and has no five-field equivalent"
				case strings.Contains(fieldErr.Error(), "hash option"):
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

func unknownTimezone(name string) error {
	return invalid(fmt.Sprintf("unknown time zone %q; use an IANA name such as America/New_York, or UTC", clip(name, 40)))
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
