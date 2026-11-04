package cron

// DecodeJob strictly decodes a stored job document without applying the
// current validation rules. Stored rows and accepted events were validated
// when written; re-validating them on read would make one row that a later
// release's rules reject break listing, export and federation for its project.
func DecodeJob(input []byte) (JobDefinition, error) {
	var job JobDefinition
	err := decode(input, &job, DefinitionLimit)
	return job, err
}

// DecodeWorkflow is DecodeJob for workflow documents.
func DecodeWorkflow(input []byte) (WorkflowDefinition, error) {
	var workflow WorkflowDefinition
	err := decode(input, &workflow, DefinitionLimit)
	return workflow, err
}

// DecodeJobForWrite admits a candidate using the save transports' required
// boolean enabled field. DecodeJob remains tolerant when reading stored intent.
func DecodeJobForWrite(input []byte) (JobDefinition, error) {
	var value struct {
		JobDefinition
		Enabled *bool `json:"enabled"`
	}
	if err := decode(input, &value, DefinitionLimit); err != nil {
		return value.JobDefinition, err
	}
	if value.Enabled == nil {
		return value.JobDefinition, invalid("enabled is required and must be boolean")
	}
	value.JobDefinition.Enabled = *value.Enabled
	return value.JobDefinition, nil
}
