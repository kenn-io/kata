package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// CronWorkflowArgvEventFeature is reserved for literal argument workflow steps.
// Recognizing a required format does not make this daemon a supported decoder;
// keep it out of SupportedEventFeatures until the complete contract is supported.
const CronWorkflowArgvEventFeature = "cron_workflow_argv_v1"

func workflowDefinitionEvent(kind string) bool {
	switch kind {
	case "cron.workflow.created", "cron.workflow.updated", "cron.workflow.deleted", "cron.workflow.restored", "cron.workflow.snapshot":
		return true
	}
	return false
}

// Classify raw envelopes independently of typed decoding: an unsupported kind
// must be fenced even when its argument member is missing or malformed.
func payloadHasArgvWorkflow(kind, payload string) (bool, error) {
	if !workflowDefinitionEvent(kind) || payload == "" {
		return false, nil
	}
	var envelope struct {
		Definition jsontext.Value `json:"definition"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return false, err
	}
	if jsontext.Value(payload).Kind() != '{' {
		return false, fmt.Errorf("workflow event payload must be an object")
	}
	if len(envelope.Definition) == 0 {
		return false, nil
	}
	if envelope.Definition.Kind() != '{' {
		return false, fmt.Errorf("workflow definition must be an object")
	}
	var definition struct {
		Steps jsontext.Value `json:"steps"`
	}
	if err := json.Unmarshal(envelope.Definition, &definition); err != nil {
		return false, err
	}
	if len(definition.Steps) == 0 {
		return false, nil
	}
	if definition.Steps.Kind() != '[' {
		return false, fmt.Errorf("workflow steps must be an array")
	}
	var steps []jsontext.Value
	if err := json.Unmarshal(definition.Steps, &steps); err != nil {
		return false, err
	}
	argv := false
	for _, raw := range steps {
		if raw.Kind() != '{' {
			return false, fmt.Errorf("workflow step must be an object")
		}
		var step struct {
			Kind jsontext.Value `json:"kind"`
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			return false, err
		}
		if len(step.Kind) == 0 {
			continue
		}
		if step.Kind.Kind() != '"' {
			return false, fmt.Errorf("workflow step kind must be a string")
		}
		var stepKind string
		if err := json.Unmarshal(step.Kind, &stepKind); err != nil {
			return false, err
		}
		argv = argv || stepKind == "argv"
	}
	return argv, nil
}
