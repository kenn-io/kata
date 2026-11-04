package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Backup restore preserves whole cutoff attribution and malformed blocking.
func checkCronPassoverImport(t *testing.T, _ db.Storage, b Backend) error {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "malformed"}[malformed], func(t *testing.T) {
			ctx := t.Context()
			source := b.Open(t)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			project, parent, job, progress := recurringFixture(t, source, "all")
			cutoff := progress.Anchor.Add(3 * time.Hour)
			paused := progress.Anchor.Add(time.Hour)
			value, err := json.Marshal(db.CronPassover{Version: 1, Through: cutoff, By: "root-operator", PausedAt: &paused})
			require.NoError(t, err)
			if malformed {
				value = jsontext.Value(`{"version":1,"through":null,"by":"root-operator"}`)
			}
			seedPassover(t, source, project.ID, map[string]jsontext.Value{db.CronPassoverMetadataKey: value})
			records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
			require.NoError(t, err)
			restored := b.Open(t)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
			target, err := restored.ProjectByUID(ctx, project.UID)
			require.NoError(t, err)
			var metadata map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(target.Metadata), &metadata))
			require.JSONEq(t, string(value), string(metadata[db.CronPassoverMetadataKey]), "all attribution survives")
			read, err := restored.CronJobControlRead(ctx, target.ID, job.UID)
			require.NoError(t, err)
			require.NotNil(t, read.Passover)
			require.Equal(t, malformed, read.Passover.Invalid)
			out, err := restored.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: target.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: cutoff})
			if malformed {
				require.ErrorIs(t, err, cron.ErrInvalid)
				require.Empty(t, out.Events)
			} else {
				require.NoError(t, err)
				require.Len(t, out.Events, 1, "restored cutoff clamps without children")
				restoredParent, err := restored.IssueByUID(ctx, parent.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, cutoff, fixtureProgress(t, restored, restoredParent.ID, job.UID).Through)
			}
		})
	}
	return nil
}

// Adoption must refuse before changing local identity/history when a retained
// root cutoff cannot be published by the new spoke.
func checkCronPassoverAdoption(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	for _, jobScope := range []bool{false, true} {
		for _, emptyOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("job=%t/empty=%t", jobScope, emptyOnly), func(t *testing.T) {
				p, err := s.CreateProject(ctx, fmt.Sprintf("cutoff-adoption-%t-%t", jobScope, emptyOnly))
				require.NoError(t, err)
				if jobScope {
					definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
					job, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Review", Definition: definition, Actor: "operator"})
					require.NoError(t, err)
					_, err = s.ResumeCronJob(ctx, db.ResumeCronJobIn{ProjectID: p.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator", DiscardMissed: true})
					require.NoError(t, err)
				} else {
					paused, err := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
					require.NoError(t, err)
					revision := paused.NewRevision
					_, err = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", ExpectedRevision: &revision, DiscardMissed: true})
					require.NoError(t, err)
				}
				before, err := s.ProjectByID(ctx, p.ID)
				require.NoError(t, err)
				beforeID, err := s.MaxEventID(ctx)
				require.NoError(t, err)
				hubUID, err := uid.New()
				require.NoError(t, err)
				_, err = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: hubUID, ReplayHorizonEventID: 1, Actor: "operator", EmptyOnly: emptyOnly})
				require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
				require.ErrorContains(t, err, "retained cron passover")
				after, err := s.ProjectByID(ctx, p.ID)
				require.NoError(t, err)
				require.Equal(t, before, after)
				afterID, err := s.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, beforeID, afterID)
				_, err = s.FederationBindingByProject(ctx, p.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
			})
		}
	}
	return nil
}
