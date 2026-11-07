package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronTallyConflictHTTP(t *testing.T) {
	for _, operation := range []string{"merge", "adoption"} {
		t.Run(operation, func(t *testing.T) {
			env := testenv.New(t)
			ctx := t.Context()
			target := seedProject(t, env, "target-project")
			group := cronHTTPUID(t)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			value := func(count int64) jsontext.Value {
				raw, err := json.Marshal(db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": {Kind: "job", UID: group, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: db.CronTallyCounts{Succeeded: count}}}})
				require.NoError(t, err)
				return raw
			}
			targetMetadata, err := json.Marshal(map[string]jsontext.Value{db.CronTallyPrefix + group: value(7)})
			require.NoError(t, err)
			_, err = env.DB.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(targetMetadata), target.ID)
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/merge", target.ID)
			headers := map[string]string{}
			var body any
			if operation == "merge" {
				source := seedProject(t, env, "source-project")
				sourceMetadata, err := json.Marshal(map[string]jsontext.Value{db.CronTallyPrefix + group: value(8)})
				require.NoError(t, err)
				_, err = env.DB.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(sourceMetadata), source.ID)
				require.NoError(t, err)
				body = map[string]any{"source_project_id": source.ID, "actor": "operator"}
			} else {
				_, err = env.DB.EnableProjectFederation(ctx, target.ID, "operator")
				require.NoError(t, err)
				spoke := cronHTTPUID(t)
				grant, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &target.ID, SpokeInstanceUID: spoke, Actor: "operator", Capabilities: "push", AllowAdoptionSnapshotAuthors: true})
				require.NoError(t, err)
				payload := fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, db.CronTallyPrefix+group, value(8))
				event := db.RemoteEvent{EventUID: cronHTTPUID(t), OriginInstanceUID: spoke, ProjectUID: target.UID, ProjectName: target.Name, Type: "project.metadata_updated", Actor: "operator", HLCPhysicalMS: time.Now().UnixMilli() + 100, Payload: jsontext.Value(payload), CreatedAt: time.Now().UTC()}
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
				path = fmt.Sprintf("/api/v1/projects/%d/federation/events:ingest", target.ID)
				headers = map[string]string{"Authorization": "Bearer " + grant.Token, "X-Kata-Event-Features": db.CronEventFeature}
				body = map[string]any{"schema_version": db.CurrentSchemaVersion(), "adoption_baseline": "complete", "adoption_baseline_end_event_id": 1, "events": []any{federationIngestEnvelope(t, 1, event)}}
			}
			response, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusConflict, response.StatusCode, string(raw))
			require.Contains(t, string(raw), "cron_tally_conflict")
			unchanged, err := env.DB.ProjectByID(ctx, target.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(targetMetadata), string(unchanged.Metadata))
		})
	}
}

func TestMergeProjectPullOnlyTargetReturnsConflict(t *testing.T) {
	env := testenv.New(t)
	source := seedProject(t, env, "source-project")
	target := seedProject(t, env, "target-project")
	_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: target.UID, Enabled: true, PushEnabled: false, Actor: "operator"})
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/merge", target.ID), map[string]any{"source_project_id": source.ID, "actor": "operator"}, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, string(raw))
	require.Contains(t, string(raw), "project_merge_federation_binding")
}
