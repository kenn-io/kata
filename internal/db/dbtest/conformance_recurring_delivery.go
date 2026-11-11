package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"
	_ "time/tzdata" // Trimmed conformance test binaries need embedded data for named-zone fixtures.

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
	if metadata == nil || occurrence == nil {
		t.Fatal("expected metadata and cron.occurrence fixture objects")
	}
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
	out, err := s.RecurringCronDelivery(t.Context(), db.CronDeliveryQuery{ProjectID: p.ID, JobUID: j.UID}, now)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Delivery)
	return out[0].Delivery
}

func checkRecurringDeliveryStatus(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, j, progress := recurringFixture(t, s, "latest")
	anchor := progress.Anchor
	now := anchor.Add(5 * time.Hour)
	original := j.Definition
	var out []db.CronDeliveryJob
	var err error
	t.Run("read only", func(t *testing.T) {
		before, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		childrenBefore, err := s.ChildrenOfIssue(ctx, parent.ID)
		require.NoError(t, err)
		for range 3 {
			deliveryRead(t, s, p, j, anchor.Add(5*time.Hour))
		}
		after, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after)
		current, err := s.IssueByID(ctx, parent.ID)
		require.NoError(t, err)
		require.Equal(t, parent.Metadata, current.Metadata)
		require.Equal(t, parent.Revision, current.Revision)
		childrenAfter, err := s.ChildrenOfIssue(ctx, parent.ID)
		require.NoError(t, err)
		require.Len(t, childrenAfter, len(childrenBefore))
	})
	t.Run("state and pause reasons", func(t *testing.T) {
		d := deliveryRead(t, s, p, j, anchor)
		require.Equal(t, "active", d.State)
		require.Equal(t, "local", d.Authority)
		require.True(t, d.DeliversHere)
		require.Equal(t, anchor.Add(2*time.Hour), *d.NextDueAt)
		require.Nil(t, d.LastChild)
		require.Equal(t, "active", deliveryRead(t, s, p, j, anchor.Add(2*time.Hour+2*time.Minute-time.Nanosecond)).State)
		require.Equal(t, "behind", deliveryRead(t, s, p, j, anchor.Add(2*time.Hour+2*time.Minute)).State)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, "behind", d.State)
		require.Equal(t, 1, d.Pending.Count)
		require.Equal(t, anchor.Add(4*time.Hour), *d.Pending.Oldest)
		_, err = s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
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
		j, _, err = s.PutCronJob(ctx, db.PutCronJob{UID: j.UID, ProjectID: p.ID, Name: j.Name, Definition: j.Definition, ExpectedEventUID: j.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		require.Equal(t, "disabled", deliveryRead(t, s, p, j, now).Reason)
		j, _, err = s.PutCronJob(ctx, db.PutCronJob{UID: j.UID, ProjectID: p.ID, Name: j.Name, Definition: j.Definition, ExpectedEventUID: j.DefinitionEventUID, Actor: "operator", Deleted: true})
		require.NoError(t, err)
		require.Equal(t, "job_deleted", deliveryRead(t, s, p, j, now).Reason)
		j, _, err = s.PutCronJob(ctx, db.PutCronJob{UID: j.UID, ProjectID: p.ID, Name: j.Name, Definition: j.Definition, ExpectedEventUID: j.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		current, err := s.IssueByID(ctx, parent.ID)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"cron.progress.`+j.UID+`":{"version":2}}`, parent.ID)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, "paused", d.State)
		require.Equal(t, "disabled", d.Reason)
		require.Contains(t, d.Error, "invalid recurring progress")
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, current.Metadata, parent.ID)
		j.Definition.Enabled = true
		j, _, err = s.PutCronJob(ctx, db.PutCronJob{UID: j.UID, ProjectID: p.ID, Name: j.Name, Definition: j.Definition, ExpectedEventUID: j.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"cron.progress.`+j.UID+`":{"version":2}}`, parent.ID)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, "error", d.State)
		require.Contains(t, d.Error, "invalid recurring progress")
	})
	t.Run("list isolation", func(t *testing.T) {
		// A malformed per-job checkpoint cannot abort a list with healthy jobs.
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"cron.progress.`+j.UID+`":{"version":2}}`, parent.ID)
		second, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Other parent", Author: "operator"})
		require.NoError(t, err)
		def := j.Definition
		def.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: second.UID, Title: "Other work"}
		_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Other schedule", Actor: "operator", Definition: def})
		require.NoError(t, err)
		out, err = s.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID}, now)
		require.NoError(t, err)
		if out[0].Job.UID != j.UID {
			out[0], out[1] = out[1], out[0]
		}
		require.NotEmpty(t, out[0].Delivery.Error)
		require.Empty(t, out[1].Delivery.Error)
	})
	t.Run("stored definition and recipient errors", func(t *testing.T) {
		// Restore valid progress, then evaluate an older stored definition.
		raw, err := json.Marshal(map[string]any{"cron.progress." + j.UID: progress})
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), parent.ID)
		j.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
		raw, err = json.Marshal(j.Definition)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
		d := deliveryRead(t, s, p, j, now)
		require.Equal(t, "error", d.State)
		require.NotEmpty(t, d.Error)
		require.Nil(t, d.NextDueAt)
		j.Definition = original
		raw, err = json.Marshal(original)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
		deliveryExec(t, s, `UPDATE issues SET owner=NULL WHERE id=$1`, parent.ID)
		require.Empty(t, deliveryRead(t, s, p, j, now).Error)
		blank := "   "
		badParent, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Bad recipient", Author: "operator", Owner: &blank})
		require.NoError(t, err)
		badDefinition := original
		badDefinition.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: badParent.UID, Title: "Recipient work"}
		badJob, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Recipient schedule", Actor: "operator", Definition: badDefinition})
		require.NoError(t, err)
		d = deliveryRead(t, s, p, badJob, now)
		require.Equal(t, "error", d.State)
		require.NotNil(t, d.Pending, "a successful planner batch remains visible when recipient resolution fails")
		require.Nil(t, d.NextDueAt)
	})
	t.Run("deleted parent", func(t *testing.T) {
		_, _, _, err = s.SoftDeleteIssue(ctx, parent.ID, "operator")
		require.NoError(t, err)
		require.Equal(t, "parent_deleted", deliveryRead(t, s, p, j, now).Reason)
		_, err = s.PurgeIssue(ctx, parent.ID, "operator", nil)
		require.NoError(t, err)
		require.Equal(t, "parent_missing", deliveryRead(t, s, p, j, now).Reason)
	})
	t.Run("adapter omitted", func(t *testing.T) {
		adapterDefinition := original
		adapterDefinition.Action = cron.Action{Kind: "execute", Prompt: "Inspect"}
		adapterDefinition.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Adapter work"}
		_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Adapter", Actor: "operator", Definition: adapterDefinition})
		require.NoError(t, err)
		out, err = s.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID}, now)
		require.NoError(t, err)
		found := false
		for _, row := range out {
			if row.Job.Definition.Action.Kind == "execute" {
				found = true
				require.Nil(t, row.Delivery)
			}
		}
		require.True(t, found)
	})
	t.Run("durable scan", func(t *testing.T) {
		parent, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Scan parent", Author: "operator"})
		require.NoError(t, err)
		def := original
		def.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Scan work"}
		j, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Scan schedule", Actor: "operator", Definition: def})
		require.NoError(t, err)
		parent, err = s.IssueByID(ctx, parent.ID)
		require.NoError(t, err)
		var parentMetadata map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &parentMetadata))
		progress, err := cron.DecodeRecurringProgress(parentMetadata["cron.progress."+j.UID])
		require.NoError(t, err)
		j.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 * * * *"}
		raw, err := json.Marshal(j.Definition)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
		now := progress.Anchor.Add(60 * 24 * time.Hour)
		d := deliveryRead(t, s, p, j, now)
		require.Equal(t, "behind", d.State)
		require.Nil(t, d.Scan, "a hypothetical scan must not masquerade as durable progress")
		require.Equal(t, progress.Through, *d.Through)
		require.Zero(t, d.Pending.Count)
		require.True(t, d.Pending.More)
		_, err = s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
		require.NoError(t, err)
		stored, err := s.IssueByID(t.Context(), parent.ID)
		require.NoError(t, err)
		var metadata map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &metadata))
		checkpoint, err := cron.DecodeRecurringProgress(metadata["cron.progress."+j.UID])
		require.NoError(t, err)
		require.NotNil(t, checkpoint.Scan)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, checkpoint.Through, *d.Through)
		require.Equal(t, checkpoint.Scan.Cursor, d.Scan.Cursor)
		require.Equal(t, checkpoint.Scan.Until, d.Scan.Until)
		require.Equal(t, 1, d.Pending.Count)
		require.False(t, d.Pending.More)
	})
	return nil
}

// Delivery evidence survives replication and restore and respects child visibility.
func checkRecurringDeliveryEvidence(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, parent, j, progress := recurringFixture(t, s, "all")
	now := progress.Anchor.Add(5 * time.Hour)
	_, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
	require.NoError(t, err)
	children, err := s.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 2)
	t.Run("occurrence versions", func(t *testing.T) {
		second, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Healthy parent", Author: "operator"})
		require.NoError(t, err)
		definition := j.Definition
		definition.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: second.UID, Title: "Healthy work"}
		healthy, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Healthy schedule", Actor: "operator", Definition: definition})
		require.NoError(t, err)
		newest := deliveryRead(t, s, p, j, now).LastChild.UID
		older := children[0]
		if older.UID == newest {
			older = children[1]
		}
		var metadata, occurrence map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(older.Metadata), &metadata))
		require.NoError(t, json.Unmarshal(metadata["cron.occurrence"], &occurrence))
		occurrence["version"] = jsontext.Value("2")
		occurrence["scheduled_at"] = jsontext.Value("false")
		metadata["cron.occurrence"], err = json.Marshal(occurrence)
		require.NoError(t, err)
		raw, err := json.Marshal(metadata)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), older.ID)
		d := deliveryRead(t, s, p, j, now)
		require.Equal(t, "error", d.State)
		require.Nil(t, d.LastChild)
		rows, err := s.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID}, now)
		require.NoError(t, err)
		found := false
		for _, row := range rows {
			if row.Job.UID == healthy.UID {
				found = true
				require.Empty(t, row.Delivery.Error)
			}
		}
		require.True(t, found)
		for _, version := range []string{"1e0", "1.0"} {
			occurrence["version"] = jsontext.Value(version)
			occurrence["scheduled_at"] = jsontext.Value(`"2099-01-01T00:00:00Z"`)
			metadata["cron.occurrence"], err = json.Marshal(occurrence)
			require.NoError(t, err)
			raw, err = json.Marshal(metadata)
			require.NoError(t, err)
			deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), older.ID)
			require.Equal(t, newest, deliveryRead(t, s, p, j, now).LastChild.UID)
		}
		occurrence["version"] = jsontext.Value("9223372036854775808")
		metadata["cron.occurrence"], err = json.Marshal(occurrence)
		require.NoError(t, err)
		raw, err = json.Marshal(metadata)
		require.NoError(t, err)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), older.ID)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, "error", d.State)
		require.Nil(t, d.LastChild)
		records, err := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		restored := b.Open(t)
		t.Cleanup(func() { require.NoError(t, restored.Close()) })
		require.NoError(t, restored.ImportReplay(t.Context(), records, db.ImportOptions{}))
		rp, err := restored.ProjectByUID(t.Context(), p.UID)
		require.NoError(t, err)
		require.Equal(t, "error", deliveryRead(t, restored, rp, j, now).State)
		deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, older.Metadata, older.ID)
	})
	t.Run("last child", func(t *testing.T) {
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
		for i, stamp := range []string{"2026-10-01T09:00:00.123456789Z", "2026-10-01T09:00:00.123456788Z"} {
			parsed, err := time.Parse(time.RFC3339Nano, stamp)
			require.NoError(t, err)
			deliveryStamp(t, s, children[i], parsed)
		}
		require.Equal(t, children[0].UID, deliveryRead(t, s, p, j, now).LastChild.UID)
		readCtx := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{p.UID}))
		_, err = s.RecurringCronDelivery(readCtx, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: j.UID}, now)
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{parent.ID, children[0].ID}, db.ExposedIssues(readCtx))
		tied, err := time.Parse(time.RFC3339Nano, "2026-10-01T09:00:00.123456789Z")
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
		out, err := s.RecurringCronDelivery(scoped, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: j.UID}, now)
		require.NoError(t, err)
		require.Equal(t, children[1].UID, out[0].Delivery.LastChild.UID)
		require.ElementsMatch(t, []int64{parent.ID, children[1].ID}, db.ExposedIssues(scoped))
		deliveryExec(t, s, `UPDATE projects SET deleted_at=$1 WHERE id=$2`, "2026-01-01T00:00:00Z", hidden.ID)
		require.Equal(t, children[1].UID, deliveryRead(t, s, p, j, now).LastChild.UID)
		deliveryExec(t, s, `UPDATE projects SET deleted_at=NULL WHERE id=$1`, hidden.ID)
		moved, err := s.IssueByID(ctx, children[0].ID)
		require.NoError(t, err)
		_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: hidden.ID, ToProjectID: p.ID, IfMatchRev: moved.Revision, Actor: "operator"})
		require.NoError(t, err)
		_, _, err = s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: hidden.ID, Actor: "operator", Force: true})
		require.NoError(t, err)
		for _, child := range children {
			deliveryExec(t, s, `UPDATE issues SET metadata=$1 WHERE id=$2`, child.Metadata, child.ID)
		}
	})
	t.Run("replica", func(t *testing.T) {
		binding, err := s.EnableProjectFederation(ctx, p.ID, "operator")
		require.NoError(t, err)
		page, err := s.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, AfterID: binding.ReplayHorizonEventID - 1, Features: db.SupportedEventFeatures, Limit: 100})
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
		h := deliveryRead(t, s, p, j, now)
		require.Equal(t, "hub", h.Authority)
		require.True(t, h.DeliversHere)
		require.Equal(t, "spoke", d.Authority)
		require.False(t, d.DeliversHere)
		require.Equal(t, h.Through, d.Through)
		require.Equal(t, h.LastChild, d.LastChild)
		deliveryExec(t, s, `UPDATE federation_bindings SET enabled=0 WHERE project_id=$1`, p.ID)
		d = deliveryRead(t, s, p, j, now)
		require.Equal(t, "hub_disabled", d.Authority)
		require.Equal(t, "not_owning_hub", d.Reason)
	})
	return nil
}

// CheckCronDeliveryConsistent commits delivery and a definition change after the parent read.
func CheckCronDeliveryConsistent(t *testing.T, s db.Storage, setStage func(func(context.Context) error)) {
	p, _, j, progress := recurringFixture(t, s, "all")
	now := progress.Anchor.Add(5 * time.Hour)
	setStage(func(ctx context.Context) error {
		setStage(nil)
		if _, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now}); err != nil {
			return err
		}
		changed := j.Definition
		changed.Enabled = false
		raw, err := json.Marshal(changed)
		if err != nil {
			return err
		}
		_, err = s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		}).ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), j.UID)
		return err
	})
	rows, err := s.RecurringCronDelivery(t.Context(), db.CronDeliveryQuery{ProjectID: p.ID, JobUID: j.UID}, now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].Job.Definition.Enabled)
	require.Equal(t, progress.Through, *rows[0].Delivery.Through)
	require.Nil(t, rows[0].Delivery.LastChild)
	after := deliveryRead(t, s, p, j, now)
	require.Equal(t, "disabled", after.Reason)
	require.Equal(t, now, *after.Through)
	require.NotNil(t, after.LastChild)
}
