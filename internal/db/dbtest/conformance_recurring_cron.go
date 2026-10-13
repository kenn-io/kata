package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/uid"
)

// Delivery evidence cannot be fabricated or rewound by ordinary writes.
func checkRecurringMetadataProtection(t *testing.T, s db.Storage) error {
	p, e := s.CreateProject(t.Context(), "recurring-project")
	require.NoError(t, e)
	parent, _, e := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Ongoing commitment", Author: "operator"})
	require.NoError(t, e)
	for _, key := range []string{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV", "cron.occurrence"} {
		_, _, e = s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Fabricated evidence", Author: "operator", Metadata: map[string]jsontext.Value{key: []byte(`{"version":1}`)}})
		require.Error(t, e, key)
		for _, value := range []string{`null`, `{"version":1,"anchor":"2026-01-01T00:00:00Z","through":"2026-01-01T00:00:00Z"}`} {
			_, e = s.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: parent.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: []byte(value)}})
			require.Error(t, e, key)
		}
	}
	return nil
}

func recurringFixture(t *testing.T, s db.Storage, catchup string) (db.Project, db.Issue, db.CronJob, cron.RecurringProgress) {
	t.Helper()
	p, e := s.CreateProject(t.Context(), "recurring-project")
	require.NoError(t, e)
	owner := "worker/child"
	parent, _, e := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Ongoing commitment", Author: "operator", Owner: &owner})
	require.NoError(t, e)
	j := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review service"}, Overlap: "allow", Catchup: catchup}
	job, events, e := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Periodic review", Actor: "operator", Definition: j})
	require.NoError(t, e)
	require.Len(t, events, 2)
	parent, e = s.IssueByID(t.Context(), parent.ID)
	require.NoError(t, e)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &values))
	var progress cron.RecurringProgress
	require.NoError(t, json.Unmarshal(values["cron.progress."+job.UID], &progress))
	require.NoError(t, progress.Validate())
	return p, parent, job, progress
}

func checkRecurringCron(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, parent, job, progress := recurringFixture(t, s, "all")
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(10 * 24 * time.Hour)}
	t.Run("LongOutageConcurrentRetry", func(t *testing.T) {
		other := b.OpenSame(t, s)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, store := range []db.Storage{s, other} {
			wg.Go(func() { _, e := store.DispatchRecurringCron(ctx, in); errs <- e })
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			require.NoError(t, e)
		}
		for {
			out, e := s.DispatchRecurringCron(ctx, in)
			require.NoError(t, e)
			if !out.More {
				break
			}
		}
		children, e := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, e)
		require.Len(t, children, 121)
		seen := map[string]bool{}
		for _, child := range children {
			if child.ID == parent.ID {
				continue
			}
			var m map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(child.Metadata), &m))
			var occurrence struct {
				ScheduledAt time.Time `json:"scheduled_at"`
			}
			require.NoError(t, json.Unmarshal(m["cron.occurrence"], &occurrence))
			key := occurrence.ScheduledAt.Format(time.RFC3339Nano)
			require.False(t, seen[key])
			seen[key] = true
			require.Equal(t, parent.Owner, child.Owner)
			require.Contains(t, m, notification.MetadataKey("worker/child"))
			require.NotContains(t, m, "scheduled_on")
			links, e := s.LinksByIssue(ctx, child.ID)
			require.NoError(t, e)
			require.Len(t, links, 1)
			require.Equal(t, "parent", links[0].Type)
		}
	})
	t.Run("AcknowledgementStaysCleared", func(t *testing.T) {
		children, e := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, e)
		for _, child := range children {
			if child.ID == parent.ID {
				continue
			}
			_, e = s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: child.ID, Actor: "worker", Patch: map[string]jsontext.Value{notification.MetadataKey("worker/child"): []byte(`null`)}})
			require.NoError(t, e)
		}
		out, e := s.DispatchRecurringCron(ctx, in)
		require.NoError(t, e)
		require.Empty(t, out.Events)
		ids, e := s.ListDueNotificationIssueIDs(ctx)
		require.NoError(t, e)
		require.Empty(t, ids)
		children, e = s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, e)
		require.Len(t, children, 121)
		for _, child := range children {
			require.NotContains(t, string(child.Metadata), notification.MetadataKey("worker/child"))
		}
	})
	t.Run("RecipientAndLifecycleRace", func(t *testing.T) {
		nextOwner := "next-worker"
		_, _, _, e := s.UpdateOwner(ctx, parent.ID, &nextOwner, "operator")
		require.NoError(t, e)
		stale := in
		stale.ExpectedEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		stale.Now = in.Now.Add(2 * time.Hour)
		out, e := s.DispatchRecurringCron(ctx, stale)
		require.NoError(t, e)
		require.Empty(t, out.Events)
		in.Now = stale.Now
		out, e = s.DispatchRecurringCron(ctx, in)
		require.NoError(t, e)
		require.Len(t, out.Events, 2)
		children, e := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, e)
		require.Len(t, children, 122)
		child, e := s.IssueByUID(ctx, *out.Events[0].IssueUID, db.IncludeDeletedNo)
		require.NoError(t, e)
		require.Equal(t, "next-worker", *child.Owner)
		require.Contains(t, string(child.Metadata), notification.MetadataKey("next-worker"))
		job.Definition.Enabled = false
		updated, _, e := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
		require.NoError(t, e)
		in.ExpectedEventUID = updated.DefinitionEventUID
		in.Now = in.Now.Add(2 * time.Hour)
		out, e = s.DispatchRecurringCron(ctx, in)
		require.NoError(t, e)
		require.Empty(t, out.Events)
		job = updated
	})
	t.Run("ImmutableBinding", func(t *testing.T) {
		replacement, _, e := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Replacement parent", Author: "operator"})
		require.NoError(t, e)
		changed := job.Definition
		changed.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Changed", ParentUID: replacement.UID}
		_, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: changed, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
		require.Error(t, e)
		unchanged, e := s.CronJob(ctx, p.ID, job.UID)
		require.NoError(t, e)
		require.NotNil(t, unchanged.Definition.Issue)
		require.Equal(t, parent.UID, unchanged.Definition.Issue.ParentUID)
	})
	t.Run("CorruptProgress", func(t *testing.T) {
		// Test-only corruption models damaged/incomplete restore, never public mutation.
		q := s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, e := q.ExecContext(ctx, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
		require.NoError(t, e)
		job.Definition.Enabled = true
		updated, _, e := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
		require.Error(t, e, "edits must not initialize missing progress")
		_ = updated
	})
	return nil
}

// A failed progress write rolls back all already-created children and their events.
func checkRecurringAtomicRollback(t *testing.T, s db.Storage, b Backend) error {
	p, parent, job, progress := recurringFixture(t, s, "all")
	ctx := t.Context()
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	var install, remove []string
	if b.Name == "postgres" {
		install = []string{`CREATE FUNCTION reject_recurring_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(parent.ID) + ` THEN RAISE EXCEPTION 'injected progress failure'; END IF; RETURN NEW; END $$`, `CREATE TRIGGER reject_recurring_progress BEFORE UPDATE OF metadata ON issues FOR EACH ROW EXECUTE FUNCTION reject_recurring_progress()`}
		remove = []string{`DROP TRIGGER reject_recurring_progress ON issues`, `DROP FUNCTION reject_recurring_progress()`}
	} else {
		install = []string{`CREATE TRIGGER reject_recurring_progress BEFORE UPDATE OF metadata ON issues WHEN NEW.id=` + fmt.Sprint(parent.ID) + ` BEGIN SELECT RAISE(FAIL,'injected progress failure'); END`}
		remove = []string{`DROP TRIGGER reject_recurring_progress`}
	}
	for _, stmt := range install {
		_, e := q.ExecContext(ctx, stmt)
		require.NoError(t, e)
	}
	before, e := s.MaxEventID(ctx)
	require.NoError(t, e)
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(4 * time.Hour)}
	out, e := s.DispatchRecurringCron(ctx, in)
	if b.Name == "postgres" {
		require.ErrorContains(t, e, "SQLSTATE P0001")
	} else {
		require.ErrorContains(t, e, "injected progress failure")
	}
	require.Empty(t, out.Events)
	children, e := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, e)
	require.Len(t, children, 1)
	after, e := s.MaxEventID(ctx)
	require.NoError(t, e)
	require.Equal(t, before, after)
	for _, stmt := range remove {
		_, e = q.ExecContext(ctx, stmt)
		require.NoError(t, e)
	}
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Len(t, out.Events, 3)
	return nil
}

func checkRecurringLifecycle(t *testing.T, s db.Storage) error {
	p, parent, job, progress := recurringFixture(t, s, "latest")
	ctx := t.Context()
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2000 * time.Hour)}
	// Closing the continuing commitment prevents delivery until it is reopened.
	_, _, _, e := s.CloseIssue(ctx, parent.ID, "done", "operator", "Recurring commitment paused", nil)
	require.NoError(t, e)
	out, e := s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	_, _, _, e = s.ReopenIssue(ctx, parent.ID, "operator")
	require.NoError(t, e)
	fixed := job.Definition
	fixed.Action.Recipient = "operator/fixed"
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: fixed, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Len(t, out.Events, 2)
	child, e := s.IssueByUID(ctx, *out.Events[0].IssueUID, db.IncludeDeletedNo)
	require.NoError(t, e)
	require.Contains(t, string(child.Metadata), notification.MetadataKey("operator/fixed"))
	require.NotContains(t, string(child.Metadata), notification.MetadataKey("worker/child"))
	// Export/replay must retain the cleared request and progress independently.
	_, e = s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: child.ID, Actor: "operator", Patch: map[string]jsontext.Value{notification.MetadataKey("operator/fixed"): []byte(`null`)}})
	require.NoError(t, e)
	records, e := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, e)
	require.NoError(t, s.ImportReplay(ctx, records, db.ImportOptions{}))
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	child, e = s.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, e)
	require.NotContains(t, string(child.Metadata), notification.MetadataKey("operator/fixed"))
	return nil
}

// A native job keeps its parent in the project that owns the recurring schedule.
func checkRecurringParentProjectMove(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, parent, _, _ := recurringFixture(t, s, "latest")
	target, err := s.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	before, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: parent.ID, FromProjectID: project.ID, ToProjectID: target.ID,
		IfMatchRev: parent.Revision, Actor: "operator",
	})
	require.ErrorContains(t, err, "native recurring job")
	moved, err := s.IssueByUID(ctx, parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, project.ID, moved.ProjectID)
	after, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "a rejected move cannot commit an event")
	return nil
}

// Native recurring issues remain ordinary issues that can join a recurrence.
func checkRecurringMetadataAttachment(t *testing.T, s db.Storage, _ Backend) error {
	ctx := t.Context()
	project, parent, job, progress := recurringFixture(t, s, "latest")
	out, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{
		ProjectID: project.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID,
		Now: progress.Anchor.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, out.Events, 2)
	child, err := s.IssueByUID(ctx, *out.Events[0].IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var childMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(child.Metadata), &childMetadata))
	require.Contains(t, childMetadata, "cron.occurrence")

	createRecurrence := func() db.CreateRecurrenceIn {
		return db.CreateRecurrenceIn{
			ProjectID: project.ID, Actor: "operator", Rule: "FREQ=DAILY",
			DTStart: "2026-10-13", Timezone: "UTC",
			Template: db.RecurrenceTemplate{Title: "Follow-up"},
		}
	}
	_, err = s.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: child.ID, Recurrence: createRecurrence()})
	require.NoError(t, err, "child occurrence metadata is not template metadata")
	child, err = s.IssueByID(ctx, child.ID)
	require.NoError(t, err)
	var attachedChildMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(child.Metadata), &attachedChildMetadata))
	require.Contains(t, attachedChildMetadata, "cron.occurrence")
	require.JSONEq(t, string(childMetadata["cron.occurrence"]), string(attachedChildMetadata["cron.occurrence"]),
		"recurrence attachment preserves occurrence provenance")

	parent, err = s.IssueByID(ctx, parent.ID)
	require.NoError(t, err)
	progressKey := "cron.progress." + job.UID
	var parentMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &parentMetadata))
	var before cron.RecurringProgress
	require.NoError(t, json.Unmarshal(parentMetadata[progressKey], &before))
	_, err = s.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: parent.ID, Recurrence: createRecurrence()})
	require.NoError(t, err, "bound parent progress is not template metadata")
	parent, err = s.IssueByID(ctx, parent.ID)
	require.NoError(t, err)
	var attachedParentMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &attachedParentMetadata))
	require.Contains(t, attachedParentMetadata, progressKey)
	var after cron.RecurringProgress
	require.NoError(t, json.Unmarshal(attachedParentMetadata[progressKey], &after))
	require.Equal(t, before, after, "recurrence attachment preserves the native job checkpoint")
	return nil
}

// Native history fences old peers before reset/cursor writes; replica state never dispatches.
func checkRecurringFederation(t *testing.T, hub db.Storage, b Backend) error {
	ctx := t.Context()
	p, _, job, progress := recurringFixture(t, hub, "latest")
	binding, e := hub.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, e)
	_, e = hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, Metadata: true, Features: db.CronEventFeature})
	require.ErrorIs(t, e, db.ErrUnsupportedEventFeatures)
	metadata, e := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, Metadata: true, Features: db.SupportedEventFeatures})
	require.NoError(t, e)
	require.NoError(t, db.RequireEventFeatures(db.SupportedEventFeatures, metadata.RequiredFeatures))
	page, e := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, AfterID: binding.ReplayHorizonEventID - 1, Features: db.SupportedEventFeatures, Limit: 100})
	require.NoError(t, e)
	require.NoError(t, db.RequireEventFeatures(db.SupportedEventFeatures, page.RequiredFeatures))
	peer := b.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	local, e := peer.CreateProject(ctx, "spoke-project")
	require.NoError(t, e)
	adopted, e := peer.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: p.ID, HubProjectUID: p.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "operator", EmptyOnly: true})
	require.NoError(t, e)
	for _, event := range page.Events {
		_, e = peer.InsertRemoteEvent(ctx, adopted.Project.ID, portableNativeEvent(event))
		require.NoError(t, e)
	}
	e = peer.MaterializeFederatedProject(ctx, adopted.Project.ID)
	require.NoError(t, e)
	candidate, e := peer.ListRecurringCronJobs(ctx, "", 100)
	require.NoError(t, e)
	require.Empty(t, candidate)
	in := db.DispatchRecurringCronIn{ProjectID: adopted.Project.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2 * time.Hour)}
	out, e := peer.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	// A retained disabled binding is still a replica, even when generic writes pass.
	q := peer.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0,push_enabled=1 WHERE project_id=$1`, adopted.Project.ID)
	require.NoError(t, e)
	out, e = peer.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	candidate, e = peer.ListRecurringCronJobs(ctx, "", 100)
	require.NoError(t, e)
	require.Empty(t, candidate)
	_, _, e = peer.PutCronJob(ctx, db.PutCronJob{ProjectID: adopted.Project.ID, Name: "Replica schedule", Actor: "operator", Definition: job.Definition})
	require.Error(t, e)
	// A new spoke mutation cannot rewind delivery metadata or change native binding.
	parentUID := job.Definition.Issue.ParentUID
	event := newRemoteEvent(t, p, &parentUID, "issue.metadata_updated", "operator", peer.InstanceUID(), 999, jsontext.Value(`{"diff":{"cron.progress.`+job.UID+`":{"to":null}},"revision_new":99,"updated_at":"2026-10-11T00:00:00Z"}`))
	before, e := hub.MaxEventID(ctx)
	require.NoError(t, e)
	_, e = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: peer.InstanceUID(), BoundActor: "operator", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.ErrorIs(t, e, db.ErrFederationIngestValidation)
	after, e := hub.MaxEventID(ctx)
	require.NoError(t, e)
	require.Equal(t, before, after)
	return nil
}

func checkRecurringOwnerRaceAndFence(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, parent, job, progress := recurringFixture(t, s, "latest")
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2 * time.Hour)}
	denied := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return errors.New("withdrawn write authority") })
	out, e := s.DispatchRecurringCron(denied, in)
	require.ErrorContains(t, e, "withdrawn write authority")
	require.Empty(t, out.Events)
	other := b.OpenSame(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	owner := "changed-worker"
	wg.Go(func() { _, _, _, e := other.UpdateOwner(ctx, parent.ID, &owner, "operator"); errs <- e })
	wg.Go(func() { _, e := s.DispatchRecurringCron(ctx, in); errs <- e })
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	issues, e := s.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, e)
	require.Len(t, issues, 2)
	for _, issue := range issues {
		if issue.ID == parent.ID {
			continue
		}
		require.NotNil(t, issue.Owner)
		require.Contains(t, []string{"worker/child", "changed-worker"}, *issue.Owner)
		require.Contains(t, string(issue.Metadata), notification.MetadataKey(*issue.Owner))
	}
	return nil
}

// Recovery retains checkpoints across tombstones, project merge and fresh-store restore.
func checkRecurringRecovery(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, parent, job, progress := recurringFixture(t, s, "latest")
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2 * time.Hour)}
	out, e := s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Len(t, out.Events, 2)
	childUID := *out.Events[0].IssueUID
	// A job tombstone pauses without discarding its existing progress.
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, Deleted: true})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	in.Now = progress.Anchor.Add(4 * time.Hour)
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	_, _, _, e = s.SoftDeleteIssue(ctx, parent.ID, "operator")
	require.NoError(t, e)
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	_, _, _, e = s.RestoreIssue(ctx, parent.ID, "operator")
	require.NoError(t, e)
	// Archive removes candidates and pauses direct dispatch without changing progress.
	_, _, e = s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "operator", Force: true})
	require.NoError(t, e)
	candidates, e := s.ListRecurringCronJobs(ctx, "", 100)
	require.NoError(t, e)
	require.Empty(t, candidates)
	out, e = s.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	_, _, _, e = s.RestoreProject(ctx, p.ID, "operator")
	require.NoError(t, e)
	target, e := s.CreateProject(ctx, "merged-project")
	require.NoError(t, e)
	_, e = s.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: p.ID, TargetProjectID: target.ID, Actor: "operator"})
	require.NoError(t, e)
	records, e := CollectImportRecords(ctx, s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, e)
	restored := b.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
	merged, e := restored.ProjectByUID(ctx, target.UID)
	require.NoError(t, e)
	in.ProjectID = merged.ID
	// The first occurrence survives a full merge/export/fresh restore without redelivery.
	in.Now = progress.Anchor.Add(2 * time.Hour)
	out, e = restored.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Empty(t, out.Events)
	child, e := restored.IssueByUID(ctx, childUID, db.IncludeDeletedNo)
	require.NoError(t, e)
	require.Equal(t, merged.ID, child.ProjectID)
	in.Now = progress.Anchor.Add(4 * time.Hour)
	out, e = restored.DispatchRecurringCron(ctx, in)
	require.NoError(t, e)
	require.Len(t, out.Events, 2)
	restoredParent, e := restored.IssueByUID(ctx, parent.UID, db.IncludeDeletedNo)
	require.NoError(t, e)
	children, e := restored.ChildrenOfIssue(ctx, restoredParent.ID)
	require.NoError(t, e)
	require.Len(t, children, 2)
	return nil
}

// Native hub-owned snapshots cannot be pushed from a spoke, so adoption must refuse before rebinding.
func checkRecurringAdoptionRefusal(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, _, job, progress := recurringFixture(t, s, "latest")
	before, e := s.MaxEventID(ctx)
	require.NoError(t, e)
	_, e = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", ReplayHorizonEventID: 1, Actor: "operator"})
	require.ErrorIs(t, e, db.ErrFederatedSpokeUnsupported)
	after, e := s.MaxEventID(ctx)
	require.NoError(t, e)
	require.Equal(t, before, after)
	retained, e := s.ProjectByID(ctx, p.ID)
	require.NoError(t, e)
	require.Equal(t, p.UID, retained.UID)
	out, e := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2 * time.Hour)})
	require.NoError(t, e)
	require.Len(t, out.Events, 2, "refused adoption retains local delivery authority and progress")
	return nil
}

// Adoption must retain hub authority when a generated child moves without its job.
func checkRecurringMovedChildAdoptionRefusal(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	source, _, job, progress := recurringFixture(t, s, "latest")
	delivered, err := s.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{
		ProjectID: source.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID,
		Now: progress.Anchor.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, delivered.Events, 2)
	child, err := s.IssueByUID(ctx, *delivered.Events[0].IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	destination, err := s.CreateProject(ctx, "moved-child-project")
	require.NoError(t, err)
	_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: child.ID, FromProjectID: source.ID, ToProjectID: destination.ID,
		IfMatchRev: child.Revision, Actor: "operator",
	})
	require.NoError(t, err)
	child, err = s.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Contains(t, child.Metadata, "cron.occurrence")

	before, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	_, err = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: destination.ID, HubURL: "https://daemon.example",
		HubProjectID: destination.ID + 1000, HubProjectUID: hubUID,
		ReplayHorizonEventID: 1, Actor: "operator", EmptyOnly: false,
	})
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
	after, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "refused adoption cannot replace local history")
	retained, err := s.ProjectByUID(ctx, destination.UID)
	require.NoError(t, err)
	require.Equal(t, destination.ID, retained.ID, "refused adoption cannot replace project identity")
	_, err = s.EnableProjectFederation(ctx, destination.ID, "operator")
	require.NoError(t, err)
	beforeRead, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = s.ReadFederation(ctx, db.FederationReadParams{ProjectID: destination.ID, Features: db.CronEventFeature, Metadata: true})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	afterRead, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, beforeRead, afterRead, "an old peer cannot advance its cursor over native issue metadata")
	return nil
}

// Coarse probes must retain escaped native JSON without decoding ordinary rows.
func checkRecurringFeatureProbeCandidates(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, err := s.CreateProject(ctx, "probe-project")
	require.NoError(t, err)
	parent, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Ongoing review", Author: "operator", Metadata: map[string]jsontext.Value{"note": []byte(`"future"`)}})
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "operator", Message: "Review service"}, Issue: &cron.IssuePolicy{Kind: "existing", UID: parent.UID}, Overlap: "allow", Catchup: "latest"}
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Adapter review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	probe := &recurringCountingProbe{recurringProbeQuerier: s.(recurringProbeQuerier)}
	features, err := db.ProjectRequiredEventFeatures(ctx, probe, p.UID)
	require.NoError(t, err)
	require.Equal(t, db.CronEventFeature, features)
	require.Equal(t, []int{0}, probe.candidates, "ordinary jobs must not reach JSON decoding")
	definition.Issue = &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review service"}
	job, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Native review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	raw, err := json.Marshal(definition)
	require.NoError(t, err)
	escaped := strings.ReplaceAll(string(raw), `"notify"`, `"\u006eotify"`)
	escaped = strings.ReplaceAll(escaped, `"per-run"`, `"\u0070er-run"`)
	_, err = s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}).ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, escaped, job.UID)
	require.NoError(t, err)
	probe.candidates = nil
	features, err = db.ProjectRequiredEventFeatures(ctx, probe, p.UID)
	require.NoError(t, err)
	require.Equal(t, db.MergeEventFeatures(db.SupportedEventFeatures), features)
	require.Equal(t, []int{1}, probe.candidates, "escaped native definition remains the only candidate")
	return nil
}

type recurringProbeQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type recurringCountingProbe struct {
	recurringProbeQuerier
	candidates []int
}

func (p *recurringCountingProbe) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := p.recurringProbeQuerier.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	p.candidates = append(p.candidates, count)
	return p.recurringProbeQuerier.QueryContext(ctx, query, args...)
}

// Recurrence writes cannot fabricate protected delivery evidence through templates.
func checkRecurringTemplateProtection(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "template-project")
	require.NoError(t, err)
	issue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Ongoing review", Author: "operator"})
	require.NoError(t, err)
	in := db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "operator", Rule: "FREQ=DAILY", DTStart: "2026-01-01", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Review service"}}
	existing, _, err := s.CreateRecurrence(ctx, in)
	require.NoError(t, err)
	for _, key := range []string{"cron.occurrence", "cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		raw, err := json.Marshal(map[string]jsontext.Value{key: []byte(`{"version":1}`)})
		require.NoError(t, err)
		value := jsontext.Value(raw)
		bad := in
		bad.Template.Metadata = value
		before, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		_, _, err = s.CreateRecurrence(ctx, bad)
		require.ErrorIs(t, err, db.ErrInvalidRecurrence)
		_, err = s.PatchRecurrence(ctx, db.PatchRecurrenceIn{RecurrenceID: existing.ID, IfMatchRev: existing.Revision, Actor: "operator", Update: db.RecurrenceUpdate{TemplateMetadata: &value}})
		require.ErrorIs(t, err, db.ErrInvalidRecurrence)
		_, err = s.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: issue.ID, Recurrence: bad})
		require.ErrorIs(t, err, db.ErrInvalidRecurrence)
		_, err = db.ComposeRecurrenceIssueMetadata(raw, "2026-01-01", "UTC")
		require.ErrorIs(t, err, db.ErrInvalidRecurrence, "stored templates must also fail closed at materialization")
		after, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after, "rejected template writes emit no events")
	}
	return nil
}

// Project metadata never holds native delivery state, so keys that resemble it
// stay ordinary project metadata for old peers and spoke pushes alike.
func checkRecurringProjectMetadataKeys(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, e := s.CreateProject(ctx, "hub-project")
	require.NoError(t, e)
	binding, e := s.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, e)
	_, e = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: p.ID, Actor: "operator", Patch: map[string]jsontext.Value{"cron.progress.custom": []byte(`"local"`)}})
	require.NoError(t, e)
	page, e := s.ReadFederation(ctx, db.FederationReadParams{ProjectID: p.ID, AfterID: binding.ReplayHorizonEventID - 1, Features: db.CronEventFeature, Limit: 100})
	require.NoError(t, e, "a peer without native recurring support can read ordinary project metadata")
	require.NotEmpty(t, page.Events)

	spokeUID, e := uid.New()
	require.NoError(t, e)
	enrollment, e := s.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spokeUID, ProjectID: &p.ID, Capabilities: "push", Actor: "operator"})
	require.NoError(t, e)
	pushed := newRemoteEvent(t, p, nil, "project.metadata_updated", "operator", spokeUID, 500,
		jsontext.Value(`{"project_uid":"`+p.UID+`","diff":{"cron.occurrence":{"from":null,"to":"spoke"}},"revision_new":3}`))
	result, e := s.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: p.ID, FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: spokeUID,
		BoundActor: "operator", EventFeatures: db.SupportedEventFeatures,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: pushed}},
	})
	require.NoError(t, e, "a spoke can push ordinary project metadata")
	require.Equal(t, 1, result.Accepted)
	return nil
}
