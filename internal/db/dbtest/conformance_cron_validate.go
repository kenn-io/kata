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
	"go.kenn.io/kata/internal/uid"
)

// Validation is a read of shared document/state preconditions, with no event,
// clock, progress or definition writes, including on failed validation.
func checkCronValidate(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := s.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 * * *"},
		Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	in := db.CronValidationInput{ProjectID: p.ID, Name: "  Review  ", Raw: raw, From: from, Count: 5}
	validate := func(in db.CronValidationInput, workflow bool) db.CronValidation {
		t.Helper()
		beforeEvent, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		before, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		var out db.CronValidation
		if workflow {
			out, err = s.ValidateCronWorkflow(ctx, in)
		} else {
			out, err = s.ValidateCronJob(ctx, in)
		}
		require.NoError(t, err)
		after, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		require.Equal(t, before, after, "validation must not write")
		afterEvent, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, beforeEvent, afterEvent, "validation emits no events or HLC ticks")
		return out
	}
	out := validate(in, false)
	require.True(t, out.Valid)
	require.Equal(t, "Review", out.Name)
	require.Len(t, out.Schedule.Next, 5)
	stored, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: out.Name, Definition: job, Actor: "operator"})
	require.NoError(t, err)
	foreign, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: other.ID, Name: "Foreign", Definition: job, Actor: "operator"})
	require.NoError(t, err)
	for _, tc := range []struct{ id, expected, stage string }{
		{"", "", ""}, {"", stored.DefinitionEventUID, "revision"},
		{stored.UID, "", "revision"}, {stored.UID, foreign.DefinitionEventUID, "revision"}, {stored.UID, stored.DefinitionEventUID, ""},
		{foreign.UID, foreign.DefinitionEventUID, "revision"},
	} {
		candidate := in
		candidate.UID, candidate.ExpectedEventUID = tc.id, tc.expected
		out = validate(candidate, false)
		require.Equal(t, tc.stage == "", out.Valid)
		if tc.stage != "" {
			require.Equal(t, tc.stage, out.Errors[0].Stage)
			require.Nil(t, out.Schedule)
		}
		if tc.id == foreign.UID {
			require.Empty(t, out.Errors[0].DefinitionEventUID)
		}
	}
	for _, tc := range []struct{ name, raw, stage string }{
		{"", `{}`, "request"}, {strings.Repeat("x", 257), `{}`, "request"}, {"Review", `null`, "document"}, {"Review", `[]`, "document"}, {"Review", `1`, "document"}, {"Review", `"text"`, "document"}, {"Review", `{}`, "document"}, {"Review", `{"unknown":true}`, "document"},
	} {
		candidate := in
		candidate.Name, candidate.Raw = tc.name, []byte(tc.raw)
		out = validate(candidate, false)
		require.False(t, out.Valid)
		require.Equal(t, tc.stage, out.Errors[0].Stage)
	}
	// A job UID must not resolve as a workflow dependency.
	job.Action = cron.Action{Kind: "execute", WorkflowUID: stored.UID}
	in.Raw, err = json.Marshal(job)
	require.NoError(t, err)
	out = validate(in, false)
	require.False(t, out.Valid)
	require.Equal(t, "dependencies", out.Errors[0].Stage)
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: in.Name, Definition: job, Actor: "operator"})
	require.EqualError(t, err, out.Errors[0].Message)

	workflow := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "prompt", Prompt: "Review"}}}
	in.Raw, err = json.Marshal(workflow)
	require.NoError(t, err)
	require.True(t, validate(in, true).Valid)
	w, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: p.ID, Name: in.Name, Definition: workflow, Actor: "operator"})
	require.NoError(t, err)
	in.UID = w.UID
	require.False(t, validate(in, true).Valid)
	in.ExpectedEventUID = w.DefinitionEventUID
	require.True(t, validate(in, true).Valid)
	in.ProjectID = other.ID
	out = validate(in, true)
	require.False(t, out.Valid)
	require.Empty(t, out.Errors[0].DefinitionEventUID)
	in.ProjectID = p.ID
	deleted, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: p.ID, UID: w.UID, ExpectedEventUID: w.DefinitionEventUID, Name: w.Name, Definition: workflow, Actor: "operator", Deleted: true})
	require.NoError(t, err)
	out = validate(in, true)
	require.False(t, out.Valid)
	require.Equal(t, deleted.DefinitionEventUID, out.Errors[0].DefinitionEventUID)
	in.UID, in.ExpectedEventUID = strings.ToLower(w.UID), deleted.DefinitionEventUID
	require.True(t, validate(in, true).Valid, "matching tombstone revision can be restored")
	in.ProjectID = 999999
	_, err = s.ValidateCronWorkflow(ctx, in)
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkCronValidateNative(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, stored, progress := recurringFixture(t, s, "latest")
	raw, err := json.Marshal(stored.Definition)
	require.NoError(t, err)
	in := db.CronValidationInput{ProjectID: p.ID, UID: stored.UID, ExpectedEventUID: stored.DefinitionEventUID, Name: stored.Name, Raw: raw, From: progress.Anchor, Count: 3}
	out, err := s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Equal(t, "stored_progress", out.Schedule.Basis)
	require.Equal(t, progress.Anchor.Add(2*time.Hour), out.Schedule.Next[0].At)
	before, err := s.IssueByID(ctx, parent.ID)
	require.NoError(t, err)
	paused := stored.Definition
	paused.Enabled = false
	pausedRaw, err := json.Marshal(paused)
	require.NoError(t, err)
	pausedIn := in
	pausedIn.Raw = pausedRaw
	pausedOut, err := s.ValidateCronJob(ctx, pausedIn)
	require.NoError(t, err)
	require.True(t, pausedOut.Valid)
	require.Equal(t, out.Schedule, pausedOut.Schedule)
	require.Equal(t, "paused", pausedOut.Warnings[0].Code)
	after, err := s.IssueByID(ctx, parent.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, _, _, err = s.CloseIssue(ctx, parent.ID, "done", "operator", "Pause the recurring commitment while reviewing it", nil)
	require.NoError(t, err)
	closed, err := s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, closed.Valid)
	require.Equal(t, out.Schedule, closed.Schedule)
	require.Equal(t, "parent_closed", closed.Warnings[0].Code)

	deleted, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: stored.UID, ExpectedEventUID: stored.DefinitionEventUID, Name: stored.Name, Definition: stored.Definition, Actor: "operator", Deleted: true})
	require.NoError(t, err)
	in.ExpectedEventUID = deleted.DefinitionEventUID
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Equal(t, "stored_progress", out.Schedule.Basis, "restore retains its anchor")
	changed := stored.Definition
	changed.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Title: "Review"}
	in.Raw, err = json.Marshal(changed)
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "ownership", out.Errors[0].Stage)
	in.Raw = raw
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	// Fixture corruption exercises the existing put contract, not a conjectured failure.
	// Database JSON-object checks permit duplicate keys, but the strict progress
	// reader rejects them. This reaches fail() with an operational decoding error.
	_, err = q.ExecContext(ctx, `UPDATE issues SET metadata='{"duplicate":0,"duplicate":1}' WHERE id=$1`, parent.ID)
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	var syntax *jsontext.SyntacticError
	require.ErrorAs(t, err, &syntax)
	require.Empty(t, out.Errors)
	_, err = q.ExecContext(ctx, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "ownership", out.Errors[0].Stage)
	require.Nil(t, out.Schedule)
	newUID, err := uid.New()
	require.NoError(t, err)
	metadata, err := json.Marshal(map[string]jsontext.Value{"cron.progress." + newUID: []byte(`{"version":1,"anchor":"2026-01-01T00:00:00Z","through":"2026-01-01T00:00:00Z"}`)})
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(metadata), parent.ID)
	require.NoError(t, err)
	in.UID, in.ExpectedEventUID = newUID, ""
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "ownership", out.Errors[0].Stage)
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: newUID, Name: stored.Name, Definition: stored.Definition, Actor: "operator"})
	require.EqualError(t, err, out.Errors[0].Message)
	adapter := stored.Definition
	adapter.Action = cron.Action{Kind: "execute", Prompt: "Review"}
	adapter.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Review"}
	adapter.Overlap = "forbid"
	converted, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: newUID, Name: stored.Name, Definition: adapter, Actor: "operator"})
	require.NoError(t, err)
	in.ExpectedEventUID = converted.DefinitionEventUID
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "ownership", out.Errors[0].Stage, "adapter-to-native conversion shares the initialization guard")
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: newUID, ExpectedEventUID: converted.DefinitionEventUID, Name: stored.Name, Definition: stored.Definition, Actor: "operator"})
	require.EqualError(t, err, out.Errors[0].Message)
	in.UID = ""
	in.ExpectedEventUID = ""
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Equal(t, "fresh_anchor", out.Schedule.Basis)
	_, err = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: p.UID, Enabled: true, Actor: "operator"})
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Contains(t, out.Warnings, db.CronValidationWarning{Code: "hub_owned", Message: "native recurring writes are committed by the owning hub"})
	return nil
}

// The explicit shared dependency contract rejects missing, foreign and archived
// endpoints before previewing. It accepts a live dependency without saving it.
func checkCronValidateDependencies(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := s.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	workflow := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "prompt", Prompt: "Review"}}}
	liveWorkflow, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review", Actor: "operator", Definition: workflow})
	require.NoError(t, err)
	foreignWorkflow, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: other.ID, Name: "Review", Actor: "operator", Definition: workflow})
	require.NoError(t, err)
	deletedWorkflow, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Archived review", Actor: "operator", Definition: workflow})
	require.NoError(t, err)
	deletedWorkflow, _, err = s.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: deletedWorkflow.UID, ExpectedEventUID: deletedWorkflow.DefinitionEventUID, Name: deletedWorkflow.Name, Actor: "operator", Definition: workflow, Deleted: true})
	require.NoError(t, err)
	liveIssue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "operator"})
	require.NoError(t, err)
	foreignIssue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Review", Author: "operator"})
	require.NoError(t, err)
	deletedIssue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Archived review", Author: "operator"})
	require.NoError(t, err)
	_, _, _, err = s.SoftDeleteIssue(ctx, deletedIssue.ID, "operator")
	require.NoError(t, err)
	missing, err := uid.New()
	require.NoError(t, err)
	for _, kind := range []string{"workflow", "issue", "parent"} {
		type endpoint struct {
			uid   string
			valid bool
		}
		endpoints := []endpoint{{missing, false}}
		if kind == "workflow" {
			endpoints = append(endpoints, endpoint{liveWorkflow.UID, true}, endpoint{foreignWorkflow.UID, false}, endpoint{deletedWorkflow.UID, false})
		} else {
			endpoints = append(endpoints, endpoint{liveIssue.UID, true}, endpoint{foreignIssue.UID, false}, endpoint{deletedIssue.UID, false})
		}
		for _, endpoint := range endpoints {
			job := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
			switch kind {
			case "workflow":
				job.Action = cron.Action{Kind: "execute", WorkflowUID: endpoint.uid}
			case "issue":
				job.Issue = &cron.IssuePolicy{Kind: "existing", UID: endpoint.uid}
			case "parent":
				job.Action = cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
				job.Trigger = cron.Trigger{Kind: "interval", IntervalSeconds: 60}
				job.Issue.ParentUID, job.Overlap = endpoint.uid, "allow"
			}
			raw, err := json.Marshal(job)
			require.NoError(t, err)
			before, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			out, err := s.ValidateCronJob(ctx, db.CronValidationInput{ProjectID: project.ID, Name: "Review", Raw: raw, From: time.Now().UTC(), Count: 1})
			require.NoError(t, err)
			require.Equal(t, endpoint.valid, out.Valid, "%s %s: %+v", kind, endpoint.uid, out.Errors)
			after, err := s.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			if !endpoint.valid {
				require.Equal(t, "dependencies", out.Errors[0].Stage)
				require.Nil(t, out.Schedule)
				_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "operator", Definition: job})
				require.EqualError(t, err, out.Errors[0].Message)
			}
		}
	}
	return nil
}
