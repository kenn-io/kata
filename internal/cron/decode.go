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
