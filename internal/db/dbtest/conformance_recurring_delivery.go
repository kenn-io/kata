package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func deliveryExec(t *testing.T, s db.Storage, query string, args ...any) {
	t.Helper()
	_, err := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}).ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func deliveryStamp(t *testing.T, s db.Storage, child db.Issue, stamp time.Time) {
	t.Helper()
	var metadata, occurrence map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(child.Metadata), &metadata))
	require.NoError(t, json.Unmarshal(metadata["cron.occurrence"], &occurrence))
	raw, err := json.Marshal(stamp.UTC())
	require.NoError(t, err)
	occurrence["scheduled_at"] = raw
	metadata["cron.occurrence"], err = json.Marshal(occurrence)
	require.NoError(t, err)
	raw, err = json.Marshal(metadata)
	require.NoError(t, err)
	deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), child.ID)
}

func deliveryRead(t *testing.T, s db.Storage, p db.Project, j db.CronJob, now time.Time) *db.RecurringDelivery {
	t.Helper()
	out, err := s.RecurringCronDelivery(t.Context(), p.ID, []db.CronJob{j}, now)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.NotNil(t, out[0])
	return out[0]
}

func checkRecurringDeliveryStatus(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, j, progress := recurringFixture(t, s, "latest")
	anchor := progress.Anchor
	d := deliveryRead(t, s, p, j, anchor)
	require.Equal(t, "active", d.State)
	require.Equal(t, "local", d.Authority)
	require.True(t, d.DeliversHere)
	require.Equal(t, anchor.Add(2*time.Hour), *d.NextDueAt)
	require.Nil(t, d.LastChild)
	require.Equal(t, "active", deliveryRead(t, s, p, j, anchor.Add(2*time.Hour+2*time.Minute-time.Nanosecond)).State)
	require.Equal(t, "behind", deliveryRead(t, s, p, j, anchor.Add(2*time.Hour+2*time.Minute)).State)
	now := anchor.Add(5 * time.Hour)
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "behind", d.State)
	require.Equal(t, 1, d.Pending.Count)
	require.Equal(t, anchor.Add(4*time.Hour), *d.Pending.Oldest)
	_, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
	require.NoError(t, err)
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "active", d.State)
	require.Equal(t, now, *d.Through)
	require.Equal(t, anchor.Add(4*time.Hour), d.LastChild.ScheduledAt)
	children, err := s.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, err)
	for _, child := range children {
		_, _, _, err = s.CloseIssue(ctx, child.ID, "done", "operator", "Completed the delivered recurring child for this fixture.", nil)
		require.NoError(t, err)
	}
	_, _, _, err = s.CloseIssue(ctx, parent.ID, "done", "operator", "Recurring parent paused after completing its work.", nil)
	require.NoError(t, err)
	require.Equal(t, "parent_not_open", deliveryRead(t, s, p, j, now).Reason)
	_, _, _, err = s.ReopenIssue(ctx, parent.ID, "operator")
	require.NoError(t, err)
	j.Definition.Enabled = false
	require.Equal(t, "disabled", deliveryRead(t, s, p, j, now).Reason)
	j.DeletedAt = &now
	require.Equal(t, "job_deleted", deliveryRead(t, s, p, j, now).Reason)
	j.DeletedAt = nil
	deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"cron.progress.`+j.UID+`":{"version":2}}`, parent.ID)
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "paused", d.State)
	require.Equal(t, "disabled", d.Reason)
	require.Contains(t, d.Error, "invalid recurring progress")
	j.Definition.Enabled = true
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "error", d.State)
	require.Contains(t, d.Error, "invalid recurring progress")
	// A malformed per-job checkpoint cannot abort a list with healthy jobs.
	deliveryExec(t, s, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
	second, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Other parent", Author: "operator"})
	require.NoError(t, err)
	def := j.Definition
	def.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: second.UID, Title: "Other work"}
	good, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Other schedule", Actor: "operator", Definition: def})
	require.NoError(t, err)
	out, err := s.RecurringCronDelivery(ctx, p.ID, []db.CronJob{j, good}, now)
	require.NoError(t, err)
	require.NotEmpty(t, out[0].Error)
	require.Empty(t, out[1].Error)
	// Restore valid progress, then evaluate otherwise unwritable stored definitions.
	raw, err := json.Marshal(map[string]any{"cron.progress." + j.UID: progress})
	require.NoError(t, err)
	deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), parent.ID)
	original := j.Definition
	j.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
	raw, err = json.Marshal(j.Definition)
	require.NoError(t, err)
	deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
	j, err = s.CronJob(ctx, p.ID, j.UID)
	require.NoError(t, err)
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "error", d.State)
	require.NotEmpty(t, d.Error)
	require.Nil(t, d.NextDueAt)
	j.Definition = original
	raw, err = json.Marshal(original)
	require.NoError(t, err)
	deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
	deliveryExec(t, s, `UPDATE issues SET owner=NULL WHERE id=$1`, parent.ID)
	require.Empty(t, deliveryRead(t, s, p, j, now).Error)
	deliveryExec(t, s, `UPDATE issues SET author=$1 WHERE id=$2`, "invalid\nrecipient", parent.ID)
	d = deliveryRead(t, s, p, j, now)
	require.Equal(t, "error", d.State)
	require.NotNil(t, d.Pending, "a successful planner batch remains visible when recipient resolution fails")
	_, _, _, err = s.SoftDeleteIssue(ctx, parent.ID, "operator")
	require.NoError(t, err)
	require.Equal(t, "parent_deleted", deliveryRead(t, s, p, j, now).Reason)
	_, err = s.PurgeIssue(ctx, parent.ID, "operator", nil)
	require.NoError(t, err)
	require.Equal(t, "parent_missing", deliveryRead(t, s, p, j, now).Reason)
	adapter := j
	adapter.Definition.Action = cron.Action{Kind: "execute", Prompt: "Inspect"}
	out, err = s.RecurringCronDelivery(ctx, p.ID, []db.CronJob{j, adapter}, now)
	require.NoError(t, err)
	require.Nil(t, out[1])
	return nil
}

func checkRecurringDeliveryLegacyJobs(t *testing.T, s db.Storage) error {
	p, _, j, progress := recurringFixture(t, s, "all")
	for _, trigger := range []cron.Trigger{{Kind: "cron", Cron: "0 9 1-7 * 2"}, {Kind: "cron", Cron: "* 9 * * *"}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "EST"}} {
		j.Definition.Trigger = trigger
		raw, err := json.Marshal(j.Definition)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
		stored, err := s.CronJob(t.Context(), p.ID, j.UID)
		require.NoError(t, err)
		d := deliveryRead(t, s, p, stored, progress.Anchor.Add(24*time.Hour))
		require.Empty(t, d.Error)
		batch, err := cron.PlanRecurring(stored.Definition, stored.DefinitionEventUID, progress, progress.Anchor.Add(24*time.Hour))
		require.NoError(t, err)
		expected := "active"
		if len(batch.Due) > 0 && !batch.Due[0].After(progress.Anchor.Add(24*time.Hour-2*time.Minute)) {
			expected = "behind"
		}
		require.Equal(t, expected, d.State)
	}
	return nil
}

func checkRecurringDeliveryLastChild(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, parent, j, progress := recurringFixture(t, s, "all")
	now := progress.Anchor.Add(5 * time.Hour)
	_, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
	require.NoError(t, err)
	children, err := s.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 2)
	// Give the smaller UID the later occurrence, using fractional stamps that do not sort lexically.
	if children[0].UID > children[1].UID {
		children[0], children[1] = children[1], children[0]
	}
	stamps := []string{"2026-10-01T09:00:00.1Z", "2026-10-01T09:00:00Z"}
	for i, child := range children {
		stamp, err := time.Parse(time.RFC3339Nano, stamps[i])
		require.NoError(t, err)
		deliveryStamp(t, s, child, stamp)
	}
	require.Equal(t, children[0].UID, deliveryRead(t, s, p, j, now).LastChild.UID)
	tied, err := time.Parse(time.RFC3339Nano, stamps[0])
	require.NoError(t, err)
	deliveryStamp(t, s, children[1], tied)
	require.Equal(t, children[1].UID, deliveryRead(t, s, p, j, now).LastChild.UID, "larger UID wins an occurrence-time tie")
	older, err := time.Parse(time.RFC3339Nano, stamps[1])
	require.NoError(t, err)
	deliveryStamp(t, s, children[1], older)

	hidden, err := s.CreateProject(ctx, "hidden-project")
	require.NoError(t, err)
	_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: children[0].ID, FromProjectID: p.ID, ToProjectID: hidden.ID, IfMatchRev: children[0].Revision, Actor: "operator"})
	require.NoError(t, err)
	scoped := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{p.UID}))
	out, err := s.RecurringCronDelivery(scoped, p.ID, []db.CronJob{j}, now)
	require.NoError(t, err)
	require.Equal(t, children[1].UID, out[0].LastChild.UID)
	_, _, err = s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: hidden.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	require.Equal(t, children[1].UID, deliveryRead(t, s, p, j, now).LastChild.UID)
	records, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	restored := b.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
	rp, err := restored.ProjectByUID(ctx, p.UID)
	require.NoError(t, err)
	require.Equal(t, children[1].UID, deliveryRead(t, restored, rp, j, now).LastChild.UID)
	return nil
}

func checkRecurringDeliveryReplica(t *testing.T, hub db.Storage, b Backend) error {
	ctx := t.Context()
	p, _, j, progress := recurringFixture(t, hub, "all")
	now := progress.Anchor.Add(5 * time.Hour)
	_, err := hub.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
	require.NoError(t, err)
	parent, err := hub.IssueByUID(ctx, j.Definition.Issue.ParentUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	children, err := hub.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 2)
	if children[0].UID > children[1].UID {
		children[0], children[1] = children[1], children[0]
	}
	for i, child := range children {
		deliveryStamp(t, hub, child, progress.Anchor.Add(time.Duration(4-2*i)*time.Hour))
	}
	require.Equal(t, children[0].UID, deliveryRead(t, hub, p, j, now).LastChild.UID)
	binding, err := hub.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, err)
	page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, AfterID: binding.ReplayHorizonEventID - 1, Features: db.SupportedEventFeatures, Limit: 100})
	require.NoError(t, err)
	peer := b.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	local, err := peer.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	adopted, err := peer.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: p.ID, HubProjectUID: p.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "operator", EmptyOnly: true})
	require.NoError(t, err)
	for _, event := range page.Events {
		_, err = peer.InsertRemoteEvent(ctx, adopted.Project.ID, portableNativeEvent(event))
		require.NoError(t, err)
	}
	require.NoError(t, peer.MaterializeFederatedProject(ctx, adopted.Project.ID))
	d := deliveryRead(t, peer, adopted.Project, j, now)
	h := deliveryRead(t, hub, p, j, now)
	require.Equal(t, "spoke", d.Authority)
	require.False(t, d.DeliversHere)
	require.Equal(t, h.Through, d.Through)
	require.Equal(t, h.LastChild, d.LastChild)
	deliveryExec(t, hub, `UPDATE federation_bindings SET enabled=0 WHERE project_id=$1`, p.ID)
	d = deliveryRead(t, hub, p, j, now)
	require.Equal(t, "hub_disabled", d.Authority)
	require.Equal(t, "not_owning_hub", d.Reason)
	return nil
}

func checkRecurringDeliveryBareDirective(t *testing.T, s db.Storage) error {
	guarded := func() (guarded bool) {
		defer func() {
			if recover() != nil {
				guarded = false
			}
		}()
		_, err := cron.ParseCron("TZ=UTC")
		return err != nil
	}()
	if !guarded {
		t.Skip("requires the bare-timezone-directive parser guard")
	}
	p, _, j, progress := recurringFixture(t, s, "all")
	good, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Healthy schedule", Actor: "operator", Definition: j.Definition})
	require.NoError(t, err)
	j.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "TZ=UTC"}
	raw, err := json.Marshal(j.Definition)
	require.NoError(t, err)
	deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
	j, err = s.CronJob(t.Context(), p.ID, j.UID)
	require.NoError(t, err)
	out, err := s.RecurringCronDelivery(t.Context(), p.ID, []db.CronJob{j, good}, progress.Anchor.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "error", out[0].State)
	require.NotEmpty(t, out[0].Error)
	require.Empty(t, out[1].Error)
	return nil
}
