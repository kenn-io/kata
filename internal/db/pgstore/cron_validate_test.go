package pgstore_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Validation must complete while a writer holds both project and parent rows.
// A FOR UPDATE or LockCronProject in its read path would wait and time out.
func TestCronValidateDoesNotLockParent(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	parent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "cron", Cron: "0 * * * *"}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "latest"}
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	peerParent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Peer parent", Author: "operator"})
	require.NoError(t, err)
	peerDefinition := definition
	peerIssue := *definition.Issue
	peerIssue.ParentUID = peerParent.UID
	peerDefinition.Issue = &peerIssue
	peer, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Peer", Actor: "operator", Definition: peerDefinition})
	require.NoError(t, err)
	holder, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	_, err = holder.ExecContext(t.Context(), `SELECT id FROM issues WHERE id IN ($1,$2) FOR UPDATE`, parent.ID, peerParent.ID)
	require.NoError(t, err)
	require.NoError(t, db.LockCronProject(t.Context(), holder, project.ID))
	raw, err := json.Marshal(definition)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := store.ValidateCronJob(ctx, db.CronValidationInput{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Raw: raw, From: time.Now(), Count: 5})
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.Equal(t, "stored_progress", out.Schedule.Basis)
	require.Len(t, out.Schedule.Collisions.Items, 1)
	require.Equal(t, peer.UID, out.Schedule.Collisions.Items[0].Peers[0].UID)
	// Maintenance can hold an exclusive table lock. The ownership read must
	// return its deadline error through fail(), without turning it into a result.
	_, err = holder.ExecContext(t.Context(), `LOCK TABLE issues IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	blocked, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	out, err = store.ValidateCronJob(blocked, db.CronValidationInput{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Raw: raw, From: time.Now(), Count: 5})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotEmpty(t, out.Definition, "project and document stages completed before the locked ownership read")
	require.Empty(t, out.Errors)

}
