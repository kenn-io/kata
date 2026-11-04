package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Only live/enabled peers of the same delivery and project count; candidate
// updates exclude their own retained row and validation never writes.
func checkCronValidateCollisions(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := s.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
	put := func(projectID int64, name string, def cron.JobDefinition) db.CronJob {
		t.Helper()
		j, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: projectID, Name: name, Definition: def, Actor: "operator"})
		require.NoError(t, err)
		return j
	}
	candidate := put(project.ID, "Candidate", job)
	peer := put(project.ID, "Daily review", job)
	put(other.ID, "Other project", job)
	disabled := job
	disabled.Enabled = false
	put(project.ID, "Disabled", disabled)
	deleted := put(project.ID, "Deleted", job)
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: deleted.UID, ExpectedEventUID: deleted.DefinitionEventUID, Name: deleted.Name, Definition: job, Actor: "operator", Deleted: true})
	require.NoError(t, err)
	noInstant := job
	noInstant.Trigger = cron.Trigger{Kind: "interval", IntervalSeconds: 60}
	put(project.ID, "Adapter interval", noInstant)
	for _, expression := range []string{"@none", "@manual", "@triggered", "CRON_TZ=UTC @none", "TZ=UTC @manual"} {
		noInstant.Trigger = cron.Trigger{Kind: "cron", Cron: expression}
		put(project.ID, expression, noInstant)
	}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	in := db.CronValidationInput{ProjectID: project.ID, UID: candidate.UID, ExpectedEventUID: candidate.DefinitionEventUID, Name: candidate.Name, Raw: raw, From: from, Count: 1}
	before, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	out, err := s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.NotNil(t, out.Schedule.Collisions)
	clashes := out.Schedule.Collisions
	require.False(t, clashes.Truncated)
	require.Equal(t, 6, clashes.Skipped)
	require.Len(t, clashes.Items, 1)
	require.Equal(t, 7, clashes.Items[0].Occurrences)
	require.Equal(t, []cron.CollisionPeer{{UID: peer.UID, Name: peer.Name}}, clashes.Items[0].Peers)
	after, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Undecodable stored definitions are informational omissions, not errors.
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json='{"version":1,"unknown":1}' WHERE uid=$1`, peer.UID)
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Empty(t, out.Schedule.Collisions.Items)
	require.Equal(t, 7, out.Schedule.Collisions.Skipped)
	for _, trigger := range []cron.Trigger{{Kind: "manual"}, {Kind: "interval", IntervalSeconds: 60}, {Kind: "cron", Cron: "@every 1h"}} {
		job.Trigger = trigger
		in.UID = ""
		in.ExpectedEventUID = ""
		in.Raw, err = json.Marshal(job)
		require.NoError(t, err)
		out, err = s.ValidateCronJob(ctx, in)
		require.NoError(t, err)
		require.True(t, out.Valid)
		require.Nil(t, out.Schedule.Collisions)
	}
	return nil
}

// Native parent/progress omissions follow live dispatch eligibility and share
// the non-locking snapshot reads used by the core validation preflight.
func checkCronValidateNativeCollisions(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, _, candidate, progress := recurringFixture(t, s, "latest")
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, state := range []string{"open", "closed", "deleted", "missing-progress", "corrupt-progress", "non-object-progress", "missing-parent", "zz-oversized-metadata"} {
		parent, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Peer parent", Author: "operator"})
		require.NoError(t, err)
		definition := candidate.Definition
		copyIssue := *definition.Issue
		definition.Issue = &copyIssue
		definition.Issue.ParentUID = parent.UID
		peer, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: state, Definition: definition, Actor: "operator"})
		require.NoError(t, err)
		metadata, err := json.Marshal(map[string]any{"cron.progress." + peer.UID: progress})
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(metadata), parent.ID)
		require.NoError(t, err)
		switch state {
		case "closed":
			_, _, _, err = s.CloseIssue(ctx, parent.ID, "done", "operator", "Close this recurring parent for the conformance fixture", nil)
		case "deleted":
			_, _, _, err = s.SoftDeleteIssue(ctx, parent.ID, "operator")
		case "missing-progress":
			_, err = q.ExecContext(ctx, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
		case "corrupt-progress":
			_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{"cron.progress.%s":{}}`, peer.UID), parent.ID)
		case "non-object-progress":
			_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{"cron.progress.%s":[]}`, peer.UID), parent.ID)
		case "missing-parent":
			definition.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
			raw, marshalErr := json.Marshal(definition)
			require.NoError(t, marshalErr)
			_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), peer.UID)
		case "zz-oversized-metadata":
			// The bounded byte lookahead ends inside a UTF-8 rune. It must
			// truncate before attempting to decode that incomplete prefix.
			_, err = q.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"blob":"`+strings.Repeat("😀", 2<<20)+`"}`, parent.ID)
		}
		require.NoError(t, err)
	}
	raw, err := json.Marshal(candidate.Definition)
	require.NoError(t, err)
	in := db.CronValidationInput{ProjectID: project.ID, UID: candidate.UID, ExpectedEventUID: candidate.DefinitionEventUID, Name: candidate.Name, Raw: raw, From: progress.Anchor, Count: 1}
	out, err := s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.NotNil(t, out.Schedule.Collisions)
	require.Equal(t, 6, out.Schedule.Collisions.Skipped)
	require.True(t, out.Schedule.Collisions.Truncated)
	require.Len(t, out.Schedule.Collisions.Items, 1)
	require.Equal(t, "open", out.Schedule.Collisions.Items[0].Peers[0].Name)
	return nil
}

func checkCronValidateCollisionReadBounds(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
	seed, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Seed", Definition: job, Actor: "operator"})
	require.NoError(t, err)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for i := range 1025 {
		_, err = q.ExecContext(ctx, `INSERT INTO cron_jobs(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at) SELECT $1,project_id,$2,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at FROM cron_jobs WHERE uid=$3`, fmt.Sprintf("%026d", i+1000), fmt.Sprintf("Peer %04d", i), seed.UID)
		require.NoError(t, err)
	}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	in := db.CronValidationInput{ProjectID: project.ID, Name: "Candidate", Raw: raw, From: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), Count: 1}
	out, err := s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.True(t, out.Schedule.Collisions.Truncated)
	require.Equal(t, 1024, out.Schedule.Collisions.Items[0].PeerCount)
	// A prefix is now unpreviewable and large: the byte cap still applies
	// without spending occurrence calls or querying any native parent.
	large := job
	large.Trigger = cron.Trigger{Kind: "manual"}
	large.Options = []byte(`{"padding":"` + strings.Repeat("x", 250000) + `"}`)
	largeRaw, err := json.Marshal(large)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE name<$2`, string(largeRaw), "Peer 0040")
	require.NoError(t, err)
	out, err = s.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.True(t, out.Schedule.Collisions.Truncated)
	require.Less(t, out.Schedule.Collisions.Skipped, 100)
	return nil
}
