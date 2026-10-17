package cron

import (
	"fmt"
	"strconv"
	"strings"
)

// normalizeDayOfWeek folds Sunday 7 into 0 only for parsing. Expressions
// already accepted by robfig, including their wildcard bits, stay unchanged.
func normalizeDayOfWeek(expression string) (string, error) {
	fieldsExpression := expression
	directive := ""
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		var found bool
		directive, fieldsExpression, found = strings.Cut(expression, " ")
		if !found {
			return expression, nil
		}
	}
	if strings.HasPrefix(strings.TrimSpace(fieldsExpression), "@") {
		return expression, nil
	}
	fields := strings.Fields(fieldsExpression)
	if len(fields) != 5 {
		return expression, nil
	}
	elements := strings.Split(fields[4], ",")
	for i, element := range elements {
		normalized, err := normalizeDayOfWeekElement(element)
		if err != nil {
			return "", err
		}
		elements[i] = normalized
	}
	normalized := strings.Join(elements, ",")
	if normalized == fields[4] {
		return expression, nil
	}
	fields[4] = normalized
	result := strings.Join(fields, " ")
	if directive != "" {
		result = directive + " " + result
	}
	return result, nil
}

func normalizeDayOfWeekElement(element string) (string, error) {
	rangeAndStep := strings.Split(element, "/")
	endpoints := strings.Split(rangeAndStep[0], "-")
	// Leave wildcard elements and malformed shapes for robfig. In particular,
	// expanding a wildcard would lose the bit controlling day-field matching.
	if strings.HasPrefix(endpoints[0], "*") || strings.HasPrefix(endpoints[0], "?") || len(rangeAndStep) > 2 || len(endpoints) > 2 {
		return element, nil
	}
	start, err := dayOfWeekEndpoint(endpoints[0])
	if err != nil {
		return element, nil
	}
	end := start
	if len(endpoints) == 2 {
		end, err = dayOfWeekEndpoint(endpoints[1])
		if err != nil {
			return element, nil
		}
	}
	if start < 7 && end < 7 {
		return element, nil
	}
	step := 1
	if len(rangeAndStep) == 2 {
		step, err = dayOfWeekNumber(rangeAndStep[1])
		if err != nil {
			return "", err
		}
		// Match robfig's N/step convention, extending only newly accepted forms.
		if len(endpoints) == 1 {
			end = 7
		}
	}
	if end > 7 {
		return "", fmt.Errorf("end of range (%d) above maximum (7): %s", end, element)
	}
	if start > end {
		return "", fmt.Errorf("beginning of range (%d) beyond end of range (%d): %s", start, end, element)
	}
	if step == 0 {
		return "", fmt.Errorf("step of range should be a positive number: %s", element)
	}
	var days [7]bool
	// Bound the candidates rather than repeatedly adding a potentially huge step.
	for day := 0; day <= 7; day++ {
		if day >= start && day <= end && (day-start)%step == 0 {
			days[day%7] = true
		}
	}
	var values []string
	for day, included := range days {
		if included {
			values = append(values, strconv.Itoa(day))
		}
	}
	return strings.Join(values, ","), nil
}

func dayOfWeekEndpoint(value string) (int, error) {
	switch strings.ToLower(value) {
	case "sun":
		return 0, nil
	case "mon":
		return 1, nil
	case "tue":
		return 2, nil
	case "wed":
		return 3, nil
	case "thu":
		return 4, nil
	case "fri":
		return 5, nil
	case "sat":
		return 6, nil
	default:
		return dayOfWeekNumber(value)
	}
}

// Keep robfig's integer syntax and diagnostics, including signed overflow.
func dayOfWeekNumber(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("failed to parse int from %s: %s", value, err)
	}
	if number < 0 {
		return 0, fmt.Errorf("negative number (%d) not allowed: %s", number, value)
	}
	return number, nil
}
