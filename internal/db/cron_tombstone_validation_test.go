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
	var record CronJobExport
	record.Definition = stale
	record.CronDefinition = CronDefinition{ID: 1, ProjectID: 1, UID: newUID(t), Name: "Review", DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: newUID(t)}, Author: "worker", Revision: 2, CreatedAt: now, UpdatedAt: now, DeletedAt: &now}
	require.NoError(t, ValidateCronRecord(&record), "a backup's deleted definition must restore")
	record.DeletedAt = nil
	require.NoError(t, ValidateCronRecord(&record), "a backup taken under older rules must restore its live definitions")
}

func TestCronWorkflowBackupRetainsLegacyRawDocuments(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var base CronWorkflowExport
	base.CronDefinition = CronDefinition{ID: 1, ProjectID: 1, UID: newUID(t), Name: "Review", DefinitionEventUID: newUID(t), DefinitionHLC: CronDefinitionHLC{Version: 1, PhysicalMS: now.UnixMilli(), OriginInstanceUID: newUID(t)}, Author: "worker", Revision: 1, CreatedAt: now, UpdatedAt: now}
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
		_, err = PrepareProjectMergeRecords([]ImportRecord{project, &restored}, ProjectMergeOffsets{TargetProjectID: 2}, nil)
		require.Error(t, err, "merge must refuse the held definition")
	}
}

func TestWorkflowEventHistoricalAndAcceptedEntryPoints(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	projectUID := newUID(t)
	project := &ProjectExport{ID: 1, UID: projectUID}
	for _, tc := range []struct {
		name, document string
		historical     bool
	}{
		{"accepted null", `{"steps":[null]}`, false},
		{"historical null", `null`, true},
		{"historical null steps", `{"steps":null}`, true},
		{"historical null step", `{"steps":[null]}`, true},
		{"historical null kind", `{"steps":[{"kind":null}]}`, true},
		{"historical argv kind", `{"steps":[{"kind":"argv"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Review", Definition: jsontext.Value(tc.document), Author: "worker", CreatedAt: now, UpdatedAt: now})
			require.NoError(t, err)
			if tc.historical {
				event := &EventExport{ID: 1, UID: newUID(t), OriginInstanceUID: newUID(t), ProjectID: 1, Type: "cron.workflow.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now.Format(time.RFC3339Nano), Payload: payload}
				require.NoError(t, validateCronImportEvents([]ImportRecord{project, event}))
			} else {
				event := RemoteEvent{EventUID: newUID(t), OriginInstanceUID: newUID(t), ProjectUID: projectUID, Type: "cron.workflow.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: payload}
				require.ErrorIs(t, ValidateAcceptedCronEvent(event), ErrFederationIngestValidation)
			}
		})
	}
	payload, err := json.Marshal(CronDefinitionEvent{UID: newUID(t), ProjectUID: projectUID, Name: "Review", Definition: jsontext.Value(`{"steps":[{"kind":"argv","argv":["true"]}]}`), Author: "worker", CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	future := &EventExport{ID: 1, UID: newUID(t), OriginInstanceUID: newUID(t), ProjectID: 1, Type: "cron.workflow.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now.Format(time.RFC3339Nano), Payload: payload}
	require.Error(t, validateCronImportEvents([]ImportRecord{project, future}), "unknown future argv members were never accepted")
}
