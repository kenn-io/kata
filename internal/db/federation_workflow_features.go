package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/cron"
)

// CronWorkflowArgvEventFeature identifies literal argument workflow steps.
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

// ProjectWorkflowRequiredFeatures inspects retained workflow projections and
// event history without caching. It can be called with the relay transaction
// before any transport state changes. Held legacy evidence is never publishable,
// even to a peer that claims the reserved feature.
func ProjectWorkflowRequiredFeatures(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (string, error) {
	const query = `SELECT 'projection',w.definition_json FROM cron_workflows w JOIN projects p ON p.id=w.project_id
WHERE p.uid=$1 AND (w.definition_json LIKE '%argv%' OR w.definition_json LIKE '%null%' OR w.definition_json LIKE '%\u%' ESCAPE '!')
UNION ALL
SELECT e.type,e.payload FROM events e JOIN projects p ON p.id=e.project_id
WHERE p.uid=$1 AND e.type IN ('cron.workflow.created','cron.workflow.updated','cron.workflow.deleted','cron.workflow.restored','cron.workflow.snapshot')
AND (e.payload LIKE '%argv%' OR e.payload LIKE '%null%' OR e.payload LIKE '%\u%' ESCAPE '!')`
	rows, err := q.QueryContext(ctx, query, projectUID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			return "", err
		}
		if kind == "projection" {
			kind, payload = "cron.workflow.snapshot", `{"definition":`+payload+`}`
		}
		argv, held := retainedWorkflowEvidence(kind, payload)
		if held {
			return "", ErrHeldWorkflowEvidence
		}
		found = found || argv
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if found {
		return CronWorkflowArgvEventFeature, nil
	}
	return "", nil
}

// Keep historical decode-only warnings unchanged. Only the reserved kind and
// the explicit null shapes held by the old decoder affect negotiation.
func retainedWorkflowEvidence(kind, payload string) (bool, bool) {
	if !workflowDefinitionEvent(kind) {
		return false, false
	}
	var envelope struct {
		Definition jsontext.Value `json:"definition"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return false, false
	}
	definition := envelope.Definition
	if definition.Kind() == 'n' {
		return false, true
	}
	if definition.Kind() != '{' {
		return false, false
	}
	var body struct {
		Steps jsontext.Value `json:"steps"`
	}
	if err := json.Unmarshal(definition, &body); err != nil {
		return false, false
	}
	if body.Steps.Kind() == 'n' {
		return false, true
	}
	if body.Steps.Kind() != '[' {
		return false, false
	}
	var steps []jsontext.Value
	if err := json.Unmarshal(body.Steps, &steps); err != nil {
		return false, false
	}
	reserved := false
	for _, raw := range steps {
		if raw.Kind() == 'n' {
			return false, true
		}
		var step struct {
			Kind jsontext.Value `json:"kind"`
			Argv jsontext.Value `json:"argv"`
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			continue
		}
		if step.Kind.Kind() == 'n' {
			return false, true
		}
		var stepKind string
		if err := json.Unmarshal(step.Kind, &stepKind); err == nil && stepKind != "argv" && len(step.Argv) != 0 {
			return false, true
		}
		if stepKind == "argv" {
			reserved = true
			if len(step.Argv) == 0 || step.Argv.Kind() == 'n' {
				return false, true
			}
		}
	}
	argv, err := payloadHasArgvWorkflow(kind, payload)
	if err != nil {
		return false, reserved
	}
	if argv {
		if _, err := cron.DecodeWorkflow(definition); err != nil {
			return false, true
		}
	}
	return argv, false
}

// RequireLocalProjectWorkflows protects retained evidence before destructive replacement.
func RequireLocalProjectWorkflows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) error {
	required, err := ProjectWorkflowRequiredFeatures(ctx, q, projectUID)
	if err != nil {
		return err
	}
	return RequireEventFeatures(SupportedEventFeatures, required)
}
