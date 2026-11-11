package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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

// DecodeWorkflow is DecodeJob for workflow documents, except that the reserved
// argv kind is explicitly unsupported, including in historical stored rows.
func DecodeWorkflow(input []byte) (WorkflowDefinition, error) {
	workflow, err := DecodeHistoricalWorkflow(input)
	if err != nil {
		return workflow, err
	}
	if err := checkWorkflowNullShapes(input); err != nil {
		return workflow, err
	}
	return workflow, CheckWorkflowSupported(workflow)
}

// DecodeHistoricalWorkflow decodes the bounded shape accepted before the
// argv and null fences. It is only for retaining owner history, not execution.
func DecodeHistoricalWorkflow(input []byte) (WorkflowDefinition, error) {
	var workflow WorkflowDefinition
	err := decode(input, &workflow, DefinitionLimit)
	return workflow, err
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

// CheckWorkflowSupported rejects reserved kinds without revalidating historical
// authoring rules. It also applies to already-decoded backup records.
func CheckWorkflowSupported(workflow WorkflowDefinition) error {
	for _, step := range workflow.Steps {
		if step.Kind == "argv" {
			return invalid("argv workflow steps are not supported")
		}
	}
	return nil
}
