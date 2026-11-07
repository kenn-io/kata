package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/cron"
)

// cronResponsibilityResponse is a CLI presentation projection, not a change to
// stored definitions or REST responses. Raw values retain unknown fields and
// exact option numbers. It uses only evidence in this response, never history.
func cronResponsibilityResponse(raw []byte, resource string, list bool) ([]byte, error) {
	var body map[string]jsontext.Value
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("cron %s response must be a JSON object", resource)
	}
	key := resource
	if list {
		key += "s"
		var records []jsontext.Value
		if err := json.Unmarshal(body[key], &records); err != nil {
			return nil, err
		}
		for i, record := range records {
			projected, err := cronRecordResponsibility(record, resource)
			if err != nil {
				return nil, err
			}
			records[i] = projected
		}
		value, err := json.Marshal(records)
		if err != nil {
			return nil, err
		}
		body[key] = value
	} else {
		value, err := cronRecordResponsibility(body[key], resource)
		if err != nil {
			return nil, err
		}
		body[key] = value
	}
	return json.Marshal(body)
}

func cronRecordResponsibility(raw jsontext.Value, resource string) (jsontext.Value, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("cron %s response must be an object", resource)
	}
	var responsibility cron.Responsibility
	if resource == "run" {
		var run struct {
			Actor         string  `json:"actor"`
			ExecutorLabel *string `json:"executor_label"`
		}
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		responsibility = cron.RunResponsibility(run.Actor, run.ExecutorLabel)
	} else {
		var definition cron.JobDefinition
		if err := json.Unmarshal(fields["definition"], &definition); err != nil {
			return nil, err
		}
		native := resource == "job" && cron.IsRecurringIssueJob(definition)
		responsibility = cron.DefinitionResponsibility(definition.Options, native)
	}
	value, err := json.Marshal(responsibility)
	if err != nil {
		return nil, err
	}
	fields["responsibility"] = value
	return json.Marshal(fields)
}
