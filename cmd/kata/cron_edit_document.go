package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/cron"
)

const cronEditConflictMarker = "// CONFLICT: delete this line to save your draft over the newer version"

// cronEditParse shares update's strict request shape, then applies domain
// validation without letting editor whitespace consume the document limit.
func cronEditParse(resource string, raw []byte) (map[string]jsontext.Value, error) {
	return parseCronEdit(resource, raw, false)
}

// cronEditParseStored permits opening only never-firing jobs for repair.
// Edited drafts still use cronEditParse and must pass every rule.
func cronEditParseStored(resource string, raw []byte) (map[string]jsontext.Value, error) {
	return parseCronEdit(resource, raw, true)
}

func parseCronEdit(resource string, raw []byte, stored bool) (map[string]jsontext.Value, error) {
	if len(raw) > cronDefinitionRequestLimit {
		return nil, cronCLIValidation("cron request exceeds its byte limit")
	}
	o := cronCLIOptions{resource: resource, action: "update"}
	fields, err := o.decodeRequest(raw)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "name" && key != "definition" {
			return nil, cronCLIValidation("edit owns " + key + "; the buffer may contain only name and definition")
		}
	}
	name, err := cronStringField(fields, "name")
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 {
		return nil, cronCLIValidation("name must contain 1–256 UTF-8 bytes after trimming")
	}
	definition := fields["definition"].Clone()
	if err := definition.Compact(); err != nil {
		return nil, cronCLIValidation("invalid cron definition: " + err.Error())
	}
	var value any
	if resource == "job" {
		value, err = cron.ParseJob(definition)
	} else {
		value, err = cron.ParseWorkflow(definition)
	}
	if err != nil && (!stored || resource != "job" || !errors.Is(err, cron.ErrNeverFires)) {
		return nil, cronCLIValidation(err.Error())
	}
	// Normalize known fields as the daemon does. Options stay raw, including
	// empty objects and numeric tokens beyond float64's precision.
	fields["definition"], err = json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := setCronStringField(fields, "name", name); err != nil {
		return nil, err
	}
	return fields, nil
}

func cronEditEqual(a, b []byte) bool {
	left, right := jsontext.Value(a).Clone(), jsontext.Value(b).Clone()
	options := []jsontext.Options{jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)}
	return left.Canonicalize(options...) == nil && right.Canonicalize(options...) == nil && bytes.Equal(left, right)
}

// cronEditSplit recognizes comments only in the consecutive leading block.
// Comments within the JSON body remain JSON syntax errors, not instructions.
func cronEditSplit(raw []byte) (body []byte, conflict bool) {
	for bytes.HasPrefix(raw, []byte("//")) {
		line, rest, found := bytes.Cut(raw, []byte("\n"))
		if strings.TrimSpace(strings.TrimPrefix(string(line), "//")) == strings.TrimPrefix(cronEditConflictMarker, "// ") {
			conflict = true
		}
		if !found {
			return nil, conflict
		}
		raw = rest
	}
	return raw, conflict
}

func cronEditBuffer(resource, identity, event string, body []byte, notes []string) []byte {
	var header strings.Builder
	fmt.Fprintf(&header, "// kata cron %s edit %s (event %s). Lines starting with // at the top are ignored.\n// Delete everything below to abort.\n", resource, identity, event)
	for _, note := range notes {
		for line := range strings.SplitSeq(note, "\n") {
			line = cronEditShortLine(line)
			if header.Len()+len(line)+4 > 32*1024 {
				break
			}
			header.WriteString("// " + line + "\n")
		}
	}
	return append([]byte(header.String()), body...)
}

func cronEditShortLine(line string) string {
	if len(line) > 384 {
		return strings.ToValidUTF8(line[:381], "") + "..."
	}
	return line
}

func cronEditPretty(raw []byte) []byte {
	pretty := jsontext.Value(raw).Clone()
	if err := pretty.Indent(jsontext.WithIndent("  ")); err == nil && len(pretty) <= cronDefinitionRequestLimit {
		return pretty
	}
	return raw
}

// cronLineDiff bounds work before allocation. Large changes use a linear
// removal/addition fallback, and every path bounds displayed lines and bytes.
func cronLineDiff(a, b []string) []string {
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	var lines []string
	count := 0
	add := func(prefix, line string) {
		count++
		if len(lines) < 79 {
			lines = append(lines, cronEditShortLine(prefix+line))
		}
	}
	const cellBudget = 250_000
	if len(a)+1 > cellBudget/(len(b)+1) {
		for _, line := range a {
			add("- ", line)
		}
		for _, line := range b {
			add("+ ", line)
		}
	} else {
		width := len(b) + 1
		cells := make([]int, (len(a)+1)*width)
		for i := range slices.Backward(a) {
			for j := range slices.Backward(b) {
				if a[i] == b[j] {
					cells[i*width+j] = 1 + cells[(i+1)*width+j+1]
				} else {
					cells[i*width+j] = max(cells[(i+1)*width+j], cells[i*width+j+1])
				}
			}
		}
		for i, j := 0, 0; i < len(a) || j < len(b); {
			switch {
			case i < len(a) && j < len(b) && a[i] == b[j]:
				i, j = i+1, j+1
			case i < len(a) && (j == len(b) || cells[(i+1)*width+j] >= cells[i*width+j+1]):
				add("- ", a[i])
				i++
			default:
				add("+ ", b[j])
				j++
			}
		}
	}
	if count > len(lines) {
		lines = append(lines, fmt.Sprintf("... %d more lines; see kata cron <kind> show", count-len(lines)))
	}
	return lines
}
