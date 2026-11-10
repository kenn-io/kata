package cron

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/textsafe"
)

var (
	dayOfMonthExtension = regexp.MustCompile(`(?i)(^|,)(L(-[0-9]+)?|LW|[0-9]+W)($|,)`)
	dayOfWeekExtension  = regexp.MustCompile(`(?i)(^|,)([0-9]+|SUN|MON|TUE|WED|THU|FRI|SAT)(L|#(L|[0-9]*))($|,)`)
	hashExtension       = regexp.MustCompile(`(?i)(^|,)H($|[(/,])`)
)

// explainCron adds hints to parser errors without interpreting its error text.
func explainCron(expression string, err error) error {
	spec := expression
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		_, spec, _ = strings.Cut(spec, " ")
	}
	fields := strings.Fields(spec)
	var hint string
	switch {
	case len(fields) == 6 || len(fields) == 7:
		hint = "Kata uses five fields: minute, hour, day of month, month, and day of week"
	case spec == "@reboot":
		hint = "@reboot is not supported; use a schedule or interval trigger"
	case spec == "@every" || strings.HasPrefix(spec, "@every "):
		hint = "for native recurring jobs, use an interval trigger"
	case len(fields) == 5 && (dayOfMonthExtension.MatchString(fields[2]) || dayOfWeekExtension.MatchString(fields[4])):
		hint = "L, W, and # syntax is not supported"
	case len(fields) == 5:
		for _, field := range fields {
			if hashExtension.MatchString(field) {
				hint = "hashed schedules (H) are not supported; choose a fixed value"
				break
			}
		}
	}
	detail := safeCronDetail(err.Error(), 250)
	if hint != "" {
		detail = fmt.Sprintf("%s; %s", detail, hint)
	}
	return invalid("invalid cron expression: " + detail)
}

// safeCronDetail bounds parser errors and escapes control characters.
func safeCronDetail(s string, n int) string {
	var out strings.Builder
	for _, r := range s {
		part := string(r)
		if r < 0x20 || textsafe.IsUnsafeTerminalRune(r) {
			quoted := strconv.QuoteRune(r)
			part = quoted[1 : len(quoted)-1]
		}
		out.WriteString(part)
	}
	value, _ := textsafe.TruncateBytes(out.String(), n)
	return value
}
