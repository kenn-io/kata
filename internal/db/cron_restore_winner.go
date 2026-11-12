package db

import (
	"encoding/json/jsontext"

	"go.kenn.io/kata/internal/cron"
)

// ReconcileOwnerCronWorkflowReplay prevents a retained held event from
// reviving an older runnable projection after a full owner restore. Call only
// after validating the original records; project merges retain their policy.
func ReconcileOwnerCronWorkflowReplay(records []ImportRecord) []ImportRecord {
	projects := make(map[int64]string)
	for _, record := range records {
		if project, ok := record.(*ProjectExport); ok {
			projects[project.ID] = project.UID
		}
	}
	type key struct{ project, workflow string }
	type winner struct {
		value    CronDefinition
		document jsontext.Value
	}
	winners := make(map[key]winner)
	for _, record := range records {
		event, ok := record.(*EventExport)
		if !ok || !workflowDefinitionEvent(event.Type) {
			continue
		}
		projectUID := projects[event.ProjectID]
		in, value, err := parseCronDefinitionEvent(FoldEvent{UID: event.UID, ProjectUID: projectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type})
		if err != nil {
			continue
		} // Original input has already been validated.
		k := key{projectUID, in.UID}
		current, exists := winners[k]
		order := compareClock(cronDefinitionClock(value), cronDefinitionClock(current.value))
		// Tied evidence names one revision; unsupported originals outrank their normalized snapshot in any order.
		if !exists || order > 0 || (order == 0 && workflowDocumentHeld(string(in.Definition)) && !workflowDocumentHeld(string(current.document))) {
			winners[k] = winner{value: value, document: in.Definition}
		}
	}
	result := make([]ImportRecord, 0, len(records))
	for _, record := range records {
		row, ok := record.(*CronWorkflowExport)
		if !ok {
			result = append(result, record)
			continue
		}
		latest, exists := winners[key{projects[row.ProjectID], row.UID}]
		order := compareClock(cronDefinitionClock(latest.value), cronDefinitionClock(row.CronDefinition))
		if !exists || order < 0 {
			result = append(result, record)
			continue
		}
		definition, err := cron.DecodeHistoricalWorkflow(latest.document)
		if err != nil || definition.Version != 1 {
			continue
		}
		held := workflowDocumentHeld(string(latest.document))
		if order == 0 && (!held || sameRetainedDocument(row.RawDefinition, latest.document)) {
			result = append(result, record)
			continue
		}
		replacement := *row
		replacement.CronDefinition, replacement.Author = latest.value, row.Author
		replacement.ID, replacement.ProjectID, replacement.Revision = row.ID, row.ProjectID, row.Revision
		replacement.Definition, replacement.RawDefinition = definition, nil
		if held {
			replacement.RawDefinition = append(jsontext.Value(nil), latest.document...)
		}
		result = append(result, &replacement)
	}
	return result
}

func sameRetainedDocument(a, b jsontext.Value) bool {
	a, b = append(jsontext.Value(nil), a...), append(jsontext.Value(nil), b...)
	return len(a) != 0 && a.Canonicalize() == nil && b.Canonicalize() == nil && string(a) == string(b)
}
