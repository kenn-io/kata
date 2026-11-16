package cron

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/textsafe"
)

var hashExtension = regexp.MustCompile(`(?i)(^|,)H($|[(/,])`)

// explainCron adds hints to parser errors without interpreting its error text.
// For L, W, #, and H the parser says which option would enable the syntax;
// Kata never enables them, so the hint replaces that text.
func explainCron(expression string, err error) error {
	_, spec, _, _ := splitCronDirective(expression)
	fields := strings.Fields(spec)
	fiveFields := len(fields) == 5 && !strings.HasPrefix(spec, "@")
	detail := safeCronDetail(err.Error())
	switch {
	case (len(fields) == 6 || len(fields) == 7) && !strings.HasPrefix(spec, "@"):
		detail += "; Kata uses five fields: minute, hour, day of month, month, and day of week"
	case spec == "@reboot":
		detail += "; @reboot is not supported; use a schedule or interval trigger"
	case spec == "@every" || strings.HasPrefix(spec, "@every "):
		detail += "; adapter jobs can use @every 1s or longer; native recurring jobs use an interval trigger"
	// No valid day of month contains L or W, and no valid day of week contains
	// L or #; day names (SUN through SAT) have neither.
	case fiveFields && (strings.ContainsAny(strings.ToUpper(fields[2]), "LW") || strings.ContainsAny(strings.ToUpper(fields[4]), "L#")):
		detail = "L, W, and # syntax is not supported"
	case fiveFields && slices.ContainsFunc(fields, hashExtension.MatchString):
		detail = "hashed schedules (H) are not supported; choose a fixed value"
	}
	return invalid("invalid cron expression: " + detail)
}

// safeCronDetail escapes control characters in parser errors.
func safeCronDetail(s string) string {
	var out strings.Builder
	for _, r := range s {
		part := string(r)
		if r < 0x20 || textsafe.IsUnsafeTerminalRune(r) {
			quoted := strconv.QuoteRune(r)
			part = quoted[1 : len(quoted)-1]
		}
		out.WriteString(part)
	}
	return out.String()
}
