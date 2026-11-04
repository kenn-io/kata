package tui

import "strings"

type cronSearch struct {
	text   string
	tokens map[string][]string
}

// cronQuery keeps literal searches byte-for-byte when no recognized field is
// present. Repeated keys OR; independent keys and remaining free text AND.
func cronQuery(query string, tab int) cronSearch {
	parsed := cronSearch{text: query, tokens: map[string][]string{}}
	free := []string{}
	for word := range strings.FieldsSeq(query) {
		key, value, ok := strings.Cut(word, ":")
		key = strings.ToLower(key)
		known := false
		switch tab {
		case cronJobs:
			known = key == "health" || key == "status" || key == "action" || key == "trigger"
		case cronRuns:
			known = key == "status" || key == "job" || key == "actor" || key == "executor" || key == "teammate" || key == "issue"
		}
		if !ok || !known || value == "" {
			free = append(free, word)
			continue
		}
		value = strings.ToLower(value)
		if key == "health" {
			value = strings.ReplaceAll(value, "-", " ")
		}
		parsed.tokens[key] = append(parsed.tokens[key], value)
	}
	if len(parsed.tokens) > 0 {
		parsed.text = strings.Join(free, " ")
	}
	return parsed
}

func (q cronSearch) match(fields map[string]string, text string) bool {
	if !cronMatches(text, q.text) {
		return false
	}
	for key, values := range q.tokens {
		field := strings.ToLower(fields[key])
		matches := false
		for _, value := range values {
			switch key {
			case "status", "health":
				matches = field == value
			case "issue":
				if value == "none" {
					matches = field == ""
				} else {
					matches = strings.HasPrefix(field, value)
				}
			default:
				matches = strings.Contains(field, value)
			}
			if matches {
				break
			}
		}
		if !matches {
			return false
		}
	}
	return true
}
