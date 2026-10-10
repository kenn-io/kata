package federation

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest/faultsql"
	"go.kenn.io/kata/internal/testenv"
)

type referenceReadFaultStore struct {
	db.Storage
	pool *sql.DB
}

func (s referenceReadFaultStore) IngestFederationEvents(ctx context.Context, in db.FederationIngestParams) (db.FederationIngestResult, error) {
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return db.FederationIngestResult{}, db.NewCronReplayValidator(tx, false).Validate(ctx, in.ProjectID, in.Events[0].Event)
}

func TestCronReferenceReadFailureIsNotPoisonedPush(t *testing.T) {
	failure := errors.New("reference read unavailable")
	pool := faultsql.Open(t, &faultsql.Script{Query: func(int64) (driver.Rows, error) { return nil, failure }})
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DB = referenceReadFaultStore{Storage: cfg.DB, pool: pool} })
	project, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), project.ID, "worker")
	require.NoError(t, err)
	origin := "01M40000000000000000000006"
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "test-push-token", SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push", Actor: "worker"})
	require.NoError(t, err)
	job, version := "01M40000000000000000000001", "01M40000000000000000000002"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := db.CronRun{UID: "01M40000000000000000000004", ProjectID: project.ID, JobUID: &job, DefinitionEventUID: &version, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	payload, err := json.Marshal(db.NewCronRunObservation(run, project.UID))
	require.NoError(t, err)
	envelope := api.FederationIngestEventEnvelope{EventID: 1, EventUID: "01M40000000000000000000005", OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron.run.observed", Actor: "worker", HLCPhysicalMS: at.UnixMilli(), Payload: payload, CreatedAt: at}
	envelope.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: envelope.EventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, Type: envelope.Type, Actor: envelope.Actor, HLCPhysicalMS: at.UnixMilli(), Payload: payload, CreatedAt: at.Format(db.EventTimestampFormat)})
	require.NoError(t, err)
	body, err := json.Marshal(api.FederationIngestEventsRequestBody{SchemaVersion: 31, Events: []api.FederationIngestEventEnvelope{envelope}})
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/federation/events:ingest", env.URL, project.ID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+enrollment.Token)
	req.Header.Set("X-Kata-Event-Features", db.CronEventFeature)
	response, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	pushError := &HubStatusError{StatusCode: response.StatusCode, Body: string(raw)}
	t.Logf("actual HTTP %d, code %s, poisoned %v", response.StatusCode, federationHubErrorCode(string(raw)), isPoisonedFederationPushError(pushError))
	require.False(t, isPoisonedFederationPushError(pushError), "an operational read failure must not select persistent push quarantine")
	require.Equalf(t, http.StatusInternalServerError, response.StatusCode, "%s", raw)
	require.NotEqual(t, "validation", federationHubErrorCode(string(raw)))
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
