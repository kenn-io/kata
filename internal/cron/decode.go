package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// DecodeJob strictly decodes a stored job document without applying the
// current validation rules. Stored rows and accepted events were validated
// when written; re-validating them on read would make one row that a later
// release's rules reject break listing, export and federation for its project.
func DecodeJob(input []byte) (JobDefinition, error) {
	var job JobDefinition
	err := decode(input, &job, DefinitionLimit)
	return job, err
}

// DecodeWorkflow reads stored workflows and rejects unsupported shapes and kinds.
func DecodeWorkflow(input []byte) (WorkflowDefinition, error) {
	var workflow WorkflowDefinition
	err := decode(input, &workflow, DefinitionLimit)
	if err != nil {
		return workflow, err
	}
	if err := checkWorkflowNullShapes(input); err != nil {
		return workflow, err
	}
	if err := CheckWorkflowRawSteps(input); err != nil {
		return workflow, err
	}
	return workflow, CheckWorkflowSupported(workflow)
}

// CheckWorkflowRawSteps checks exact JSON member presence and argv element types.
// Call it before typed decoding can discard nulls or explicitly empty members.
func CheckWorkflowRawSteps(input []byte) error {
	var document struct {
		Steps []jsontext.Value `json:"steps"`
	}
	if err := json.Unmarshal(input, &document); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, raw := range document.Steps {
		if err := CheckWorkflowStepJSON(raw); err != nil {
			return err
		}
	}
	return nil
}

// CheckWorkflowStepJSON checks the raw argv shape of one workflow step.
func CheckWorkflowStepJSON(raw []byte) error {
	var step map[string]jsontext.Value
	if err := json.Unmarshal(raw, &step); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var kind string
	if rawKind, ok := step["kind"]; ok {
		if err := json.Unmarshal(rawKind, &kind); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	argv, present := step["argv"]
	if kind != "argv" {
		if present {
			return invalid("argv member requires argv kind")
		}
		return nil
	}
	if _, ok := step["command"]; ok {
		return invalid("argv step cannot carry command")
	}
	if _, ok := step["prompt"]; ok {
		return invalid("argv step cannot carry prompt")
	}
	if !present || argv.Kind() != '[' {
		return invalid("argv step requires an array")
	}
	var items []jsontext.Value
	if err := json.Unmarshal(argv, &items); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, item := range items {
		if item.Kind() != '"' {
			return invalid("argv elements must be strings")
		}
	}
	return nil
}

// DecodeHistoricalWorkflow accepts supported workflows and the bounded legacy
// shapes retained for owner history, without applying modern authoring rules.
func DecodeHistoricalWorkflow(input []byte) (WorkflowDefinition, error) {
	if workflow, err := DecodeWorkflow(input); err == nil {
		return workflow, nil
	}
	var workflow WorkflowDefinition
	if len(input) > DefinitionLimit {
		return workflow, invalid("document exceeds size limit")
	}
	if err := rejectWorkflowArgvMember(input); err != nil {
		return workflow, err
	}
	err := decode(input, &workflow, DefinitionLimit)
	return workflow, err
}

// The historical decoder accepted kind-only argv, but never the argv member.
// Check raw presence because null and empty arrays can disappear in typed JSON.
func rejectWorkflowArgvMember(input []byte) error {
	var document struct {
		Steps []jsontext.Value `json:"steps"`
	}
	if err := json.Unmarshal(input, &document); err != nil {
		return err
	}
	for _, raw := range document.Steps {
		var step map[string]jsontext.Value
		if err := json.Unmarshal(raw, &step); err != nil {
			return err
		}
		if _, exists := step["argv"]; exists {
			return invalid("argv workflow step member is not supported")
		}
	}
	return nil
}

// Nulls decode to zero values, but cannot pass federation feature
// classification. Reject them before admitting historical event documents.
func checkWorkflowNullShapes(input []byte) error {
	if jsontext.Value(input).Kind() == 'n' {
		return invalid("workflow definition must be an object")
	}
	var document struct {
		Steps jsontext.Value `json:"steps"`
	}
	if err := json.Unmarshal(input, &document); err != nil {
		return err
	}
	if document.Steps.Kind() == 'n' {
		return invalid("workflow steps must be an array")
	}
	if len(document.Steps) == 0 {
		return nil
	}
	var steps []jsontext.Value
	if err := json.Unmarshal(document.Steps, &steps); err != nil {
		return err
	}
	for _, raw := range steps {
		if raw.Kind() == 'n' {
			return invalid("workflow step must be an object")
		}
		var step struct {
			Kind jsontext.Value `json:"kind"`
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			return err
		}
		if step.Kind.Kind() == 'n' {
			return invalid("workflow step kind must be a string")
		}
	}
	return nil
}

// CheckWorkflowSupported checks supported versions and steps without applying
// modern authoring rules to historical documents.
func CheckWorkflowSupported(workflow WorkflowDefinition) error {
	if workflow.Version != 1 {
		return invalid("workflow version is not supported")
	}
	for _, step := range workflow.Steps {
		if step.Kind != "argv" {
			if step.Argv != nil {
				return invalid("argv member requires argv kind")
			}
			if step.Kind != "command" && step.Kind != "prompt" {
				return invalid("workflow step kind is not supported")
			}
			continue
		}
		if step.Command != "" || step.Prompt != "" || len(step.Argv) == 0 || step.Argv[0] == "" {
			return invalid("argv step requires a nonempty executable and no command or prompt")
		}
		for _, arg := range step.Argv {
			if strings.ContainsRune(arg, '\x00') {
				return invalid("argv elements cannot contain NUL")
			}
		}
	}
	return nil
}
