package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

type dayMatchQueries interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func withDayMatch(t *testing.T, j cron.JobDefinition) cron.JobDefinition {
	t.Helper()
	// Decode without current write rules, also usable against pre-field binaries.
	j.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 1-7 * 2", Timezone: "UTC"}
	raw, err := json.Marshal(j)
	require.NoError(t, err)
	body := map[string]jsontext.Value{}
	require.NoError(t, json.Unmarshal(raw, &body))
	body["trigger"] = []byte(`{"kind":"cron","cron":"0 9 1-7 * 2","timezone":"UTC","day_match":"all"}`)
	raw, err = json.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &j))
	return j
}

// Optional fields gate old readers even after removal and after a native cache
// hit, but must not become a permanent cache latch when history is replaced.
func checkDayMatchFeatures(t *testing.T, _ db.Storage, b Backend) error {
	oldNative := db.MergeEventFeatures(db.CronEventFeature, db.RecurringCronEventFeature)
	allFeatures := db.MergeEventFeatures(oldNative, "cron_day_match_v1")
	for _, warm := range []bool{false, true} {
		name := "history only"
		if warm {
			name = "warm native cache"
		}
		t.Run(name, func(t *testing.T) {
			s := b.Open(t)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			ctx := t.Context()
			p, _, job, _ := recurringFixture(t, s, "all")
			q := s.(dayMatchQueries)
			if warm {
				features, err := db.ProjectRequiredEventFeatures(ctx, q, p.UID)
				require.NoError(t, err)
				require.Equal(t, oldNative, features)
			}
			job.Definition = withDayMatch(t, job.Definition)
			job, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Actor: "operator", Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID})
			require.NoError(t, err)
			if !warm {
				job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * 2", Timezone: "UTC"}
				job, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Actor: "operator", Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID})
				require.NoError(t, err)
			}
			features, err := db.ProjectRequiredEventFeatures(ctx, q, p.UID)
			require.NoError(t, err)
			require.Equal(t, allFeatures, features)
			binding, err := s.EnableProjectFederation(ctx, p.ID, "operator")
			require.NoError(t, err)
			before, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			for _, metadata := range []bool{true, false} {
				in := db.FederationReadParams{ProjectID: p.ID, Metadata: metadata, Features: oldNative, AfterID: 0, Limit: 100}
				out, err := s.ReadFederation(ctx, in)
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				require.Equal(t, in.AfterID, out.NextAfterID)
				require.Zero(t, out.BaselineThroughEventID)
				require.Equal(t, binding.ReplayHorizonEventID, out.Binding.ReplayHorizonEventID)
				in.Features = allFeatures
				out, err = s.ReadFederation(ctx, in)
				require.NoError(t, err)
				require.Equal(t, allFeatures, out.RequiredFeatures)
				if !metadata {
					require.NotEmpty(t, out.Events)
					var usedMember bool
					for _, event := range out.Events {
						usedMember = usedMember || strings.Contains(event.Payload, `"day_match"`)
					}
					require.True(t, usedMember, "the advertised peer can replay the revision containing day_match")
				}
			}
			after, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after, "feature refusal and replay do not rewrite the baseline")
			// Model replacement with an older accepted history and definition. The
			// optional feature must disappear even after its positive query above.
			job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * 2", Timezone: "UTC"}
			raw, err := json.Marshal(job.Definition)
			require.NoError(t, err)
			_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), job.UID)
			require.NoError(t, err)
			_, err = q.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND type LIKE 'cron.job.%'`, p.ID)
			require.NoError(t, err)
			features, err = db.ProjectRequiredEventFeatures(ctx, q, p.UID)
			require.NoError(t, err)
			require.Equal(t, oldNative, features)
			_, err = s.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, Metadata: true, Features: oldNative})
			require.NoError(t, err, "ordinary native projects remain readable by existing peers")
		})
	}
	return nil
}

// An optional member in accepted adapter history is not native ownership.
func checkDayMatchAdapterAdoption(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, s, p)
	q := s.(dayMatchQueries)
	var payload string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1`, job.DefinitionEventUID).Scan(&payload))
	body := map[string]jsontext.Value{}
	require.NoError(t, json.Unmarshal([]byte(payload), &body))
	def := map[string]jsontext.Value{}
	require.NoError(t, json.Unmarshal(body["definition"], &def))
	def["trigger"] = []byte(`{"kind":"manual","day\u005fmatch":null}`)
	body["definition"], err = json.Marshal(def)
	require.NoError(t, err)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(raw), job.DefinitionEventUID)
	require.NoError(t, err)
	features, err := db.ProjectRequiredEventFeatures(ctx, q, p.UID)
	require.NoError(t, err)
	require.Equal(t, db.MergeEventFeatures(db.CronEventFeature, "cron_day_match_v1"), features)
	_, err = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", ReplayHorizonEventID: 1, Actor: "operator"})
	require.NoError(t, err)
	return nil
}

// Legacy both-day definitions deliver OR; editing them requires an explicit mode.
func checkDayMatchStoredDelivery(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, job, _ := recurringFixture(t, s, "all")
	job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 1-7 * 2", Timezone: "UTC"}
	raw, err := json.Marshal(job.Definition)
	require.NoError(t, err)
	q := s.(dayMatchQueries)
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), job.UID)
	require.NoError(t, err)
	anchor := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	progress, err := json.Marshal(cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor})
	require.NoError(t, err)
	metadata, err := json.Marshal(map[string]jsontext.Value{"cron.progress." + job.UID: progress})
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(metadata), parent.ID)
	require.NoError(t, err)
	out, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.False(t, out.More)
	children, err := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	require.Len(t, children, 11, "ten occurrences plus the parent")
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Actor: "operator", Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID})
	require.ErrorIs(t, err, cron.ErrInvalid)
	require.ErrorContains(t, err, "trigger.day_match")
	body := map[string]jsontext.Value{}
	require.NoError(t, json.Unmarshal(raw, &body))
	body["trigger"] = []byte(`{"kind":"cron","cron":"0 9 1-7 * 2","timezone":"UTC","day_match":"any"}`)
	raw, err = json.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &job.Definition))
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Actor: "operator", Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID})
	require.NoError(t, err)
	return nil
}
