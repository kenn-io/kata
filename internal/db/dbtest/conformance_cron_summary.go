package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Requirements include retained history and imported projections independently.
func checkCronSummaryFeatures(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	read := func(s db.Storage, projectID int64, want string) {
		t.Helper()
		for _, metadata := range []bool{true, false} {
			before, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			_, err = s.ReadFederation(ctx, db.FederationReadParams{ProjectID: projectID, Metadata: metadata, Features: "cron_v1", Limit: 100})
			if want == "cron_v1" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			}
			after, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
		}
		result, err := s.ReadFederation(ctx, db.FederationReadParams{ProjectID: projectID, Metadata: true, Features: "cron_run_summary_v2,cron_v1"})
		require.NoError(t, err)
		require.Equal(t, want, result.RequiredFeatures)
	}
	read(store, project.ID, "cron_v1")
	runUID, err := uid.New()
	require.NoError(t, err)
	in := db.ObserveCronRun{ProjectID: project.ID, UID: runUID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Reason: "agent_quota", Trigger: "catchup", Steps: []cron.SummaryStep{}}}
	result, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	replayed, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Run.Summary, replayed.Run.Summary)
	read(store, project.ID, "cron_run_summary_v2,cron_v1")
	records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	// Removing events models a projection-only backup/import, not new product DDL.
	projectionOnly := records[:0:0]
	for _, record := range records {
		if record.ImportKind() != "event" {
			projectionOnly = append(projectionOnly, record)
		}
	}
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, projectionOnly, db.ImportOptions{}))
	p, err := restored.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	if _, err := restored.FederationBindingByProject(ctx, p.ID); err != nil {
		_, err = restored.EnableProjectFederation(ctx, p.ID, "operator")
		require.NoError(t, err)
	}
	run, err := restored.CronRun(ctx, p.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, in.Summary, run.Summary)
	read(restored, p.ID, "cron_run_summary_v2,cron_v1")
	// Replacing the entire backup baseline with v1 projections drops only the
	// derived v2 requirement; a warmed native-state cache must not retain it.
	for i, record := range projectionOnly {
		if run, ok := record.(*db.CronRunExport); ok {
			runCopy := *run
			runCopy.Summary = cron.Summary{Version: 1}
			projectionOnly[i] = &runCopy
		}
	}
	require.NoError(t, restored.ImportReplay(ctx, projectionOnly, db.ImportOptions{}))
	_, err = restored.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, err)
	read(restored, p.ID, "cron_v1")
	in.ExpectedRevision = result.Run.Revision
	in.Summary = cron.Summary{Version: 1}
	_, err = store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	read(store, project.ID, "cron_run_summary_v2,cron_v1")
	return nil
}

func checkCronSummaryRelaySkew(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "summary-project")
	peer, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "summary-relay-fixture-token"})
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, fixture.project)
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: fixture.project.ID, UID: runUID, Actor: "relay-member", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Reason: "failed"}})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	resetter := store.(db.RelayResetStore)
	for _, support := range []string{"", "cron_v1"} {
		oldCtx := db.WithFederationEventFeatures(ctx, support)
		_, err := store.PendingRelayDeliveries(oldCtx, binding, db.RelayStreamEvent, 100)
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		_, err = resetter.CreateRelayReset(oldCtx, binding, fixture.signer)
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	}
	var emitted int
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND emitted=1`, binding).Scan(&emitted))
	require.Zero(t, emitted)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	checkpoint, err := resetter.CreateRelayReset(db.WithFederationEventFeatures(ctx, "cron_run_summary_v2,cron_v1"), binding, fixture.signer)
	require.NoError(t, err)
	pin, err := store.RootAuthority(ctx, fixture.project.UID)
	require.NoError(t, err)
	events, _, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
	found := false
	for _, event := range events {
		if event.Type != "cron.run.observed" && event.Type != "cron.run.snapshot" {
			continue
		}
		var body db.CronRunObservation
		require.NoError(t, json.Unmarshal(event.Payload, &body))
		if body.UID == runUID {
			require.Equal(t, "failed", body.Summary.Reason)
			found = true
		}
	}
	require.True(t, found, "the supported peer's signed checkpoint preserves v2 evidence")
	return nil
}

// A malformed v2 summary after a valid prefix must roll back the whole relay
// batch. The same supported evidence can then be accepted and replayed exactly.
func checkCronSummaryRelayAcceptance(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "summary-acceptance")
	peer, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "summary-acceptance-fixture-token"})
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, fixture.project)
	ids := [2]string{}
	envelopes := [2]db.RelayEnvelope{}
	summary := cron.Summary{Version: 1, Reason: "failed", Steps: []cron.SummaryStep{}}
	for i := range 2 {
		id, err := uid.New()
		require.NoError(t, err)
		ids[i] = id
		at := time.Now().UTC().Truncate(time.Millisecond)
		run := db.CronRun{UID: id, ProjectID: fixture.project.ID, Actor: "relay-member", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: summary, Revision: 1, CreatedAt: at, UpdatedAt: at}
		payload, err := json.Marshal(db.NewCronRunObservation(run, fixture.project.UID))
		require.NoError(t, err)
		if i == 1 {
			payload = []byte(strings.Replace(string(payload), `"reason":"failed"`, `"reason":"INVALID"`, 1))
		}
		event := newRemoteEvent(t, fixture.project, nil, "cron.run.observed", "relay-member", peer, at.UnixMilli()+10, jsontext.Value(payload))
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: fixture.project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: int64(i + 1), Stream: db.RelayStreamEvent, Path: []string{peer}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		envelopes[i] = envelope
	}
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, fixture.signer, "relay-member")
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes[:]})
	require.Error(t, err)
	for _, id := range ids {
		_, err := store.CronRun(ctx, fixture.project.ID, id)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for range 2 {
		accepted, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes[:1]})
		require.NoError(t, err)
		require.Equal(t, int64(1), accepted.Through)
		run, err := store.CronRun(ctx, fixture.project.ID, ids[0])
		require.NoError(t, err)
		require.Equal(t, summary, run.Summary)
	}
	return nil
}

// Candidate filtering excludes v1 history while retaining plain and escaped keys.
func checkCronSummaryProbeCandidates(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "summary-probe-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	id, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Message: "reason: timeout"}})
	require.NoError(t, err)
	// Probe both summary and native-definition candidates.
	_, err = db.ProjectRequiredEventFeatures(ctx, store.(recurringProbeQuerier), project.UID)
	require.NoError(t, err)
	probe := &recurringCountingProbe{recurringProbeQuerier: store.(recurringProbeQuerier)}
	features, err := db.ProjectRequiredEventFeatures(ctx, probe, project.UID)
	require.NoError(t, err)
	require.Equal(t, db.CronEventFeature, features)
	require.Equal(t, []int{0, 0}, probe.candidates, "v1 rows must not reach summary decoding")
	exec := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, raw := range []string{`{"version":1,"steps":[]}`, `{"version":1,"\u0073teps":[]}`} {
		_, err = exec.ExecContext(ctx, `UPDATE cron_runs SET summary_json=$1 WHERE uid=$2`, raw, id)
		require.NoError(t, err)
		probe.candidates = nil
		features, err = db.ProjectRequiredEventFeatures(ctx, probe, project.UID)
		require.NoError(t, err)
		require.Equal(t, db.MergeEventFeatures(db.CronRunSummaryV2Feature, db.CronEventFeature), features)
		require.Equal(t, []int{1, 0}, probe.candidates, "projection-only structured evidence remains a candidate")
	}
	// Retained escaped event evidence must survive a v1 projection rewrite too.
	_, err = exec.ExecContext(ctx, `UPDATE cron_runs SET summary_json=$1 WHERE uid=$2`, `{"version":1}`, id)
	require.NoError(t, err)
	_, err = exec.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE project_id=$2 AND type='cron.run.observed'`, `{"summary":{"version":1,"\u0073teps":[]}}`, project.ID)
	require.NoError(t, err)
	probe.candidates = nil
	features, err = db.ProjectRequiredEventFeatures(ctx, probe, project.UID)
	require.NoError(t, err)
	require.Equal(t, db.MergeEventFeatures(db.CronRunSummaryV2Feature, db.CronEventFeature), features)
	require.Equal(t, []int{1, 0}, probe.candidates)
	probe.candidates = nil
	require.NoError(t, db.RequireProjectEventFeatures(db.WithFederationEventFeatures(ctx, db.SupportedEventFeatures), probe, project.UID))
	require.Empty(t, probe.candidates, "a fully capable relay gate must not read candidates")
	_, err = exec.ExecContext(ctx, `UPDATE events SET type='issue.moved' WHERE project_id=$1 AND type='cron.workflow.created'`, project.ID)
	require.NoError(t, err)
	features, err = db.ProjectRequiredEventFeatures(ctx, store.(recurringProbeQuerier), project.UID)
	require.NoError(t, err)
	require.Equal(t, db.SupportedEventFeatures, features, "main's issue-move requirement unions with v2 and cron")
	return nil
}

func checkCronSummaryAdmission(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "admission-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	id, err := uid.New()
	require.NoError(t, err)
	in := db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "worker", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Reason: "failed"}}
	initial, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	revoked := errors.New("fixture authority revoked")
	_, err = store.ObserveCronRun(db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return revoked }), in)
	require.ErrorIs(t, err, revoked)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	replay, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, initial.Run, replay.Run)
	in.ExpectedRevision = initial.Run.Revision
	in.Summary.Message = "new evidence"
	_, err = store.ObserveCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronSummaryAdmissionRequired)
	changed := binding
	changed.HubProjectID++
	_, err = store.UpsertFederationBinding(ctx, changed)
	require.NoError(t, err)
	_, err = store.ObserveCronRun(db.WithCronSummaryBinding(ctx, &binding), in)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	return nil
}
