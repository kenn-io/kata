package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Owner restore retains unsupported source events, including rowless and held projections.
// Neither legacy reset nor signed installation may erase that evidence.
func checkResetRetainedWorkflowRestore(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "check", Kind: "command", Command: "true"}}}
	bad, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Future", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	createdUID := bad.DefinitionEventUID
	bad, _, err = source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: bad.UID, ExpectedEventUID: bad.DefinitionEventUID, Name: "Future", Actor: "worker", Definition: definition, Deleted: true})
	require.NoError(t, err)
	healthy, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Healthy", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	jobDefinition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := source.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Prompt", Actor: "worker", Definition: jobDefinition})
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for _, tc := range []struct {
		document string
		rows     int
	}{
		{`{"version":2,"steps":[]}`, 0},
		{`{"version":1,"steps":[{"key":"check","kind":"future-step"}]}`, 1},
	} {
		for _, operation := range []string{"legacy", "conditional", "signed"} {
			t.Run(operation+"/"+tc.document, func(t *testing.T) {
				fixture := append([]db.ImportRecord(nil), records...)
				var original []byte
				for i, record := range fixture {
					if event, ok := record.(*db.EventExport); ok && event.UID == bad.DefinitionEventUID {
						copyEvent := *event
						var body db.CronDefinitionEvent
						require.NoError(t, json.Unmarshal(event.Payload, &body))
						body.Definition = []byte(tc.document)
						copyEvent.Payload, err = json.Marshal(body)
						require.NoError(t, err)
						original = copyEvent.Payload
						fixture[i] = &copyEvent
					}
				}
				require.NotEmpty(t, original)
				target := backend.Open(t)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
				require.NoError(t, target.ImportReplay(ctx, fixture, db.ImportOptions{RecomputeEventContentHash: true}))
				restored, err := target.ProjectByUID(ctx, project.UID)
				require.NoError(t, err)
				q := target.(interface {
					QueryRowContext(context.Context, string, ...any) *sql.Row
				})
				var count int
				require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_workflows WHERE uid=$1`, bad.UID).Scan(&count))
				require.Equal(t, tc.rows, count)
				acknowledged, err := target.MaxEventID(ctx)
				require.NoError(t, err)
				_, err = target.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: restored.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true, PushCursorEventID: acknowledged, PullCursorEventID: 17, ReplayHorizonEventID: 13})
				require.NoError(t, err)
				var manifest db.RootResetManifest
				var snapshot db.RootResetSnapshot
				var translation db.RelayResetTranslation
				var bindingUID string
				if operation == "signed" {
					rootUID := "00000000000000000000000002"
					public, private, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
					require.NoError(t, target.PinRootAuthority(ctx, pin))
					bindingUID, err = uid.New()
					require.NoError(t, err)
					_, err = target.SetRelayBindingConfig(ctx, restored.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, target.InstanceUID()}, LocalActor: "worker", ResetEpoch: 1})
					require.NoError(t, err)
					root := &rootResetFixture{t: t, project: restored, pin: pin, private: private, rootUID: rootUID, relayUID: target.InstanceUID(), binding: bindingUID}
					manifest, snapshot, translation = root.checkpoint(2, 2)
				}
				before, err := target.FederationBindingByProject(ctx, restored.ID)
				require.NoError(t, err)
				maxBefore, err := target.MaxEventID(ctx)
				require.NoError(t, err)
				eventsBefore, err := target.EventsByUIDs(ctx, restored.ID, []string{createdUID, bad.DefinitionEventUID, healthy.DefinitionEventUID, job.DefinitionEventUID})
				require.NoError(t, err)
				require.Len(t, eventsBefore, 4)
				switch operation {
				case "legacy":
					err = target.ResetFederatedProject(ctx, restored.ID, 20, 19)
				case "conditional":
					err = target.ResetFederatedProjectIfNoPendingPush(ctx, restored.ID, 20, 19, target.InstanceUID(), maxBefore)
				case "signed":
					err = target.(db.RelayResetStore).InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation)
				}
				require.ErrorIs(t, err, db.ErrFederationIngestValidation)
				after, err := target.FederationBindingByProject(ctx, restored.ID)
				require.NoError(t, err)
				require.Equal(t, before, after)
				maxAfter, err := target.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, maxBefore, maxAfter)
				eventsAfter, err := target.EventsByUIDs(ctx, restored.ID, []string{createdUID, bad.DefinitionEventUID, healthy.DefinitionEventUID, job.DefinitionEventUID})
				require.NoError(t, err)
				require.Equal(t, eventsBefore, eventsAfter)
				restoredWorkflow, err := target.CronWorkflow(ctx, restored.ID, healthy.UID)
				require.NoError(t, err)
				require.Equal(t, definition, restoredWorkflow.Definition)
				restoredJob, err := target.CronJob(ctx, restored.ID, job.UID)
				require.NoError(t, err)
				require.Equal(t, jobDefinition, restoredJob.Definition)
			})
		}
	}
	return nil
}
