package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// An execute action with neither prompt nor workflow fails today's rules but
// decodes, as a row written under an older release's rules would.
const staleJobDocument = `{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`

func newUID(t *testing.T) string {
	t.Helper()
	value, err := uid.New()
	require.NoError(t, err)
	return value
}

// Today's rules apply to new content a spoke pushes to its hub. History that a
// hub or a backup already accepted, including superseded documents and
// tombstones, only has to decode, so tightening a rule never blocks a pull or
// a restore.
func TestCronHistoryIsNotRevalidatedAgainstCurrentRules(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID := newUID(t)
	definitionEvent := func(kind string, deleted bool) RemoteEvent {
		payload := CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Review", Definition: jsontext.Value(staleJobDocument), Author: "worker", CreatedAt: now, UpdatedAt: now}
		if deleted {
			payload.DeletedAt = &now
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		return RemoteEvent{EventUID: newUID(t), OriginInstanceUID: newUID(t), ProjectUID: projectUID, Type: kind, Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: jsontext.Value(raw)}
	}
	created := definitionEvent("cron.job.created", false)

	require.NoError(t, ValidateCronFederationEvent(definitionEvent("cron.job.deleted", true)),
		"deleting an older definition must propagate to the hub")
	require.ErrorIs(t, ValidateCronFederationEvent(created), ErrFederationIngestValidation,
		"a live definition pushed to a hub still has to pass current rules")
	require.NoError(t, ValidateAcceptedCronEvent(created), "a pulled historical document only has to decode")
	broken := created
	broken.Payload = jsontext.Value(`{"uid":"not-a-definition"}`)
	require.ErrorIs(t, ValidateAcceptedCronEvent(broken), ErrFederationIngestValidation, "accepted history must still decode")

	project := &ProjectExport{ID: 1, UID: projectUID}
	historical := &EventExport{ID: 1, UID: created.EventUID, OriginInstanceUID: created.OriginInstanceUID, ProjectID: 1, Type: created.Type, Actor: created.Actor, HLCPhysicalMS: created.HLCPhysicalMS, Payload: created.Payload, CreatedAt: now.Format(time.RFC3339Nano)}
	require.NoError(t, validateCronImportEvents([]ImportRecord{project, historical}), "a backup's historical creation event must restore")

	stale, err := cron.DecodeJob([]byte(staleJobDocument))
	require.NoError(t, err)
	record := CronJobExport{ID: 1, ProjectID: 1, UID: newUID(t), Name: "Review", DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: newUID(t)}, Author: "worker", Revision: 2, CreatedAt: now, UpdatedAt: now, DeletedAt: &now, Definition: stale}
	require.NoError(t, ValidateCronRecord(&record), "a backup's deleted definition must restore")
	record.DeletedAt = nil
	require.NoError(t, ValidateCronRecord(&record), "a backup taken under older rules must restore its live definitions")
}

func TestCronWorkflowBackupRejectsReservedKind(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	record := CronWorkflowExport{
		ID: 1, ProjectID: 1, UID: newUID(t), Name: "Review", DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: newUID(t)}, Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now,
		Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "argv"}}},
	}
	project := &ProjectExport{ID: 1, UID: newUID(t), Name: "example-project", CreatedAt: now.Format(time.RFC3339Nano), Revision: 1}
	records := []ImportRecord{project, &record} // Projection-only backup, without a cron event.
	for _, deleted := range []bool{false, true} {
		if deleted {
			record.DeletedAt = &now
		}
		require.Error(t, ValidateCronRecord(&record), "reserved workflow must not enter via a backup projection")
		require.Error(t, ValidateImportReplay(records, ImportOptions{}))
	}
	record.Definition.Steps[0].Kind = "command"
	require.NoError(t, ValidateCronRecord(&record), "historical content is not subject to current authoring rules")
	require.NoError(t, ValidateImportReplay(records, ImportOptions{}))
}

func TestCronWorkflowBackupRetainsLegacyRawDocuments(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	base := CronWorkflowExport{CronDefinition: CronDefinition{ID: 1, ProjectID: 1, UID: newUID(t), Name: "Review", DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: newUID(t)}, Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now}}
	for _, document := range []string{`null`, `{"steps":null}`, `{"steps":[null]}`, `{"steps":[{"kind":null}]}`, `{"version":1,"steps":[{"kind":"argv"}]}`} {
		base.RawDefinition = jsontext.Value(document)
		raw, err := json.Marshal(base)
		require.NoError(t, err)
		var restored CronWorkflowExport
		require.NoError(t, json.Unmarshal(raw, &restored, json.RejectUnknownMembers(true)))
		require.JSONEq(t, document, string(restored.RawDefinition))
		require.NoError(t, ValidateCronRecord(&restored))
		project := &ProjectExport{ID: 1, UID: newUID(t), Name: "example-project", CreatedAt: now.Format(time.RFC3339Nano), Revision: 1}
		require.NoError(t, ValidateImportReplay([]ImportRecord{project, &restored}, ImportOptions{}))
		_, err = PrepareProjectMergeRecords([]ImportRecord{project, &restored}, ProjectMergeOffsets{}, nil)
		require.Error(t, err, "merge must not make held state visible")
	}
}

func TestAcceptedWorkflowEventsRejectNullShapes(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID := newUID(t)
	for _, document := range []string{`null`, `{"steps":null}`, `{"steps":[null]}`, `{"steps":[{"kind":null}]}`} {
		payload, err := json.Marshal(CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Review", Definition: jsontext.Value(document), Author: "worker", CreatedAt: now, UpdatedAt: now})
		require.NoError(t, err)
		event := RemoteEvent{EventUID: newUID(t), OriginInstanceUID: newUID(t), ProjectUID: projectUID, Type: "cron.workflow.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: payload}
		require.ErrorIs(t, ValidateAcceptedCronEvent(event), ErrFederationIngestValidation, "null shape must not be admitted before feature classification: %s", document)
	}
}

func TestOwnerBackupRetainsHistoricalWorkflowEvents(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID := newUID(t)
	project := &ProjectExport{ID: 1, UID: projectUID}
	for _, document := range []string{`null`, `{"steps":null}`, `{"steps":[null]}`, `{"steps":[{"kind":null}]}`, `{"steps":[{"kind":"argv"}]}`, `{"steps":[{"kind":"argv","argv":["true"]}]}`} {
		payload, err := json.Marshal(CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Review", Definition: jsontext.Value(document), Author: "worker", CreatedAt: now, UpdatedAt: now})
		require.NoError(t, err)
		event := &EventExport{ID: 1, UID: newUID(t), OriginInstanceUID: newUID(t), ProjectID: 1, Type: "cron.workflow.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now.Format(time.RFC3339Nano), Payload: payload}
		require.NoError(t, validateCronImportEvents([]ImportRecord{project, event}), document)
		require.JSONEq(t, document, string(func() jsontext.Value {
			var body CronDefinitionEvent
			require.NoError(t, json.Unmarshal(event.Payload, &body))
			return body.Definition
		}()))
	}
}
