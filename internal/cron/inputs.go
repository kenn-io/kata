package cron

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"unicode"
)

var inputName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type inputDeclaration struct {
	Type        string   `json:"type"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Values      []string `json:"values,omitempty"`
	MaxLength   *int64   `json:"max_length,omitempty"`
	Multiline   bool     `json:"multiline,omitempty"`
	Min         *int64   `json:"min,omitempty"`
	Max         *int64   `json:"max,omitempty"`
}

type ResolvedInput struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Values      []string `json:"values,omitempty"`
	MaxLength   *int64   `json:"max_length,omitempty"`
	Multiline   bool     `json:"multiline,omitempty"`
	Min         *int64   `json:"min,omitempty"`
	Max         *int64   `json:"max,omitempty"`
	Value       any      `json:"value,omitempty"`
	Source      string   `json:"source"`
}
type InputError struct {
	Name    string `json:"name,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type InputResolution struct {
	DeclaredBy string          `json:"declared_by"`
	Inputs     []ResolvedInput `json:"inputs"`
	Errors     []InputError    `json:"errors"`
}

func inputObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("must be an object")
	}
	return m, nil
}
func declarations(raw json.RawMessage) (map[string]inputDeclaration, error) {
	out := map[string]inputDeclaration{}
	if raw == nil {
		return out, nil
	}
	fields, err := inputObject(raw)
	if err != nil {
		return nil, fmt.Errorf("inputs %w", err)
	}
	if len(fields) > 32 {
		return nil, fmt.Errorf("inputs exceed 32 declarations")
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		if !inputName.MatchString(name) {
			return nil, fmt.Errorf("%s: invalid input name", name)
		}
		if err := validateOptions([]byte(`{"` + name + `":null}`)); err != nil {
			return nil, fmt.Errorf("%s: forbidden input name", name)
		}
		fields, err := inputObject(fields[name])
		if err != nil {
			return nil, fmt.Errorf("%s: declaration %w", name, err)
		}
		for field, value := range fields {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("%s: %s cannot be null", name, field)
			}
		}
		rawFields, _ := json.Marshal(fields)
		dec := json.NewDecoder(bytes.NewReader(rawFields))
		dec.DisallowUnknownFields()
		dec.UseNumber()
		var d inputDeclaration
		if err = dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		if len(d.Title) > 80 || len(d.Description) > 400 {
			return nil, fmt.Errorf("%s: display text too long", name)
		}
		switch d.Type {
		case "string":
			if d.MaxLength != nil && (*d.MaxLength < 1 || *d.MaxLength > 2048) {
				return nil, fmt.Errorf("%s: invalid max_length", name)
			}
			if fields["values"] != nil || fields["min"] != nil || fields["max"] != nil {
				return nil, fmt.Errorf("%s: incompatible fields", name)
			}
		case "int":
			if d.Min != nil && d.Max != nil && *d.Min > *d.Max {
				return nil, fmt.Errorf("%s: min exceeds max", name)
			}
			if fields["values"] != nil || fields["max_length"] != nil || fields["multiline"] != nil {
				return nil, fmt.Errorf("%s: incompatible fields", name)
			}
		case "bool":
			if fields["values"] != nil || fields["max_length"] != nil || fields["multiline"] != nil || fields["min"] != nil || fields["max"] != nil {
				return nil, fmt.Errorf("%s: incompatible fields", name)
			}
		case "enum":
			if len(d.Values) < 1 || len(d.Values) > 64 || fields["max_length"] != nil || fields["multiline"] != nil || fields["min"] != nil || fields["max"] != nil {
				return nil, fmt.Errorf("%s: invalid enum", name)
			}
			seen := map[string]bool{}
			for _, v := range d.Values {
				if len(v) < 1 || len(v) > 128 || seen[v] {
					return nil, fmt.Errorf("%s: invalid enum values", name)
				}
				seen[v] = true
			}
		default:
			return nil, fmt.Errorf("%s: invalid type", name)
		}
		if fields["default"] != nil && !validInputValue(d, d.Default) {
			return nil, fmt.Errorf("%s: invalid default", name)
		}
		out[name] = d
	}
	return out, nil
}
func validInputValue(d inputDeclaration, v any) bool {
	switch d.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return false
		}
		limit := int64(256)
		if d.MaxLength != nil {
			limit = *d.MaxLength
		}
		if int64(len(s)) > limit {
			return false
		}
		if !d.Multiline {
			for _, r := range s {
				if unicode.IsControl(r) {
					return false
				}
			}
		}
		return true
	case "bool":
		_, ok := v.(bool)
		return ok
	case "enum":
		s, ok := v.(string)
		return ok && slices.Contains(d.Values, s)
	case "int":
		var n int64
		switch x := v.(type) {
		case json.Number:
			var err error
			n, err = x.Int64()
			if err != nil {
				return false
			}
		case int64:
			n = x
		default:
			return false
		}
		return (d.Min == nil || n >= *d.Min) && (d.Max == nil || n <= *d.Max)
	}
	return false
}
func inputValues(raw json.RawMessage) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("input_values must be an object")
	}
	return m, nil
}

// ValidateWorkflowInputs validates only declarations on workflow client writes.
func ValidateWorkflowInputs(options []byte) error {
	o, err := inputObject(options)
	if err != nil {
		return invalid(err.Error())
	}
	if _, err = declarations(o["inputs"]); err != nil {
		return invalid(err.Error())
	}
	return nil
}

// ValidateInputs applies the input convention at client write boundaries only.
func ValidateInputs(options []byte, workflowUID string, workflowOptions []byte) error {
	o, err := inputObject(options)
	if err != nil {
		return invalid(err.Error())
	}
	if workflowUID != "" && o["inputs"] != nil {
		return invalid("inputs: workflow jobs cannot declare inputs")
	}
	declaration := o["inputs"]
	if workflowUID != "" {
		w, e := inputObject(workflowOptions)
		if e != nil {
			return invalid(e.Error())
		}
		declaration = w["inputs"]
	}
	ds, err := declarations(declaration)
	if err != nil {
		return invalid(err.Error())
	}
	values, err := inputValues(o["input_values"])
	if err != nil {
		return invalid(err.Error())
	}
	for n, v := range values {
		d, ok := ds[n]
		if !ok {
			return invalid(n + ": undeclared input")
		}
		if !validInputValue(d, v) {
			return invalid(n + ": invalid input value")
		}
	}
	return nil
}

// ResolveInputs reports all usable values and errors without changing stored definitions.
func ResolveInputs(job JobDefinition, workflow *WorkflowDefinition) InputResolution {
	r := InputResolution{DeclaredBy: "none", Inputs: []ResolvedInput{}, Errors: []InputError{}}
	o, err := inputObject(job.Options)
	if err != nil {
		r.Errors = append(r.Errors, InputError{Code: "declaration_invalid", Message: err.Error()})
		return r
	}
	raw := o["inputs"]
	if job.Action.WorkflowUID != "" {
		if raw != nil {
			r.Errors = append(r.Errors, InputError{Code: "declaration_invalid", Message: "workflow jobs cannot declare inputs"})
		}
		raw = nil
		if workflow != nil {
			w, e := inputObject(workflow.Options)
			if e != nil {
				r.Errors = append(r.Errors, InputError{Code: "declaration_invalid", Message: e.Error()})
				return r
			}
			raw = w["inputs"]
			if raw != nil {
				r.DeclaredBy = "workflow"
			}
		}
	} else if raw != nil {
		r.DeclaredBy = "job"
	}
	ds := map[string]inputDeclaration{}
	declared := map[string]bool{}
	if raw != nil {
		fields, err := inputObject(raw)
		if err != nil || len(fields) > 32 {
			r.Errors = append(r.Errors, InputError{Code: "declaration_invalid", Message: "inputs must be an object of at most 32 declarations"})
			return r
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			declared[name] = true
			single, _ := json.Marshal(map[string]json.RawMessage{name: fields[name]})
			parsed, err := declarations(single)
			if err != nil {
				r.Errors = append(r.Errors, InputError{Name: name, Code: "declaration_invalid", Message: err.Error()})
				continue
			}
			ds[name] = parsed[name]
		}
	}
	values, err := inputValues(o["input_values"])
	if err != nil {
		r.Errors = append(r.Errors, InputError{Code: "invalid_value", Message: err.Error()})
		return r
	}
	names := make([]string, 0, len(ds))
	for n := range ds {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		d := ds[n]
		v := d.Default
		source := "unset"
		if v != nil {
			source = "default"
		}
		if fixed, ok := values[n]; ok {
			v = fixed
			source = "job"
			if !validInputValue(d, v) {
				r.Errors = append(r.Errors, InputError{Name: n, Code: "invalid_value", Message: "invalid input value"})
			}
		}
		if source == "unset" && d.Required {
			r.Errors = append(r.Errors, InputError{Name: n, Code: "missing_required", Message: "required input is unset"})
		}
		r.Inputs = append(r.Inputs, ResolvedInput{Name: n, Type: d.Type, Title: d.Title, Description: d.Description, Required: d.Required, Values: d.Values, MaxLength: d.MaxLength, Multiline: d.Multiline, Min: d.Min, Max: d.Max, Value: v, Source: source})
	}
	names = names[:0]
	for n := range values {
		if !declared[n] {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		r.Errors = append(r.Errors, InputError{Name: n, Code: "undeclared_value", Message: "undeclared input"})
	}
	return r
}
