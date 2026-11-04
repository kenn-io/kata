package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

func tallyLifecycleEntry(f cronTallyFixture, count int64) db.CronTallyEntry {
	return db.CronTallyEntry{Kind: "job", UID: f.job.UID, CoverageStartedAt: f.before, LastPrunedAt: f.before, OldestCreatedAt: f.before.Add(-time.Hour), NewestCreatedAt: f.before, Counts: db.CronTallyCounts{Succeeded: count}}
}

func checkCronTallyRootRefold(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	f.seed(ctx, t, tallyLifecycleEntry(f, 3))
	_, err := store.EnableProjectFederation(ctx, f.project.ID, "operator")
	require.NoError(t, err)
	// Current root authority can differ from its retained baseline after an
	// import/adoption/prune transaction. A baseline refresh is not a precondition
	// for preserving it during either full or incremental materialization.
	f.seed(ctx, t, tallyLifecycleEntry(f, 9))
	require.NoError(t, store.MaterializeFederatedProject(ctx, f.project.ID))
	project, err := store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	values, err := db.DecodeCronTallies(project.Metadata)
	require.NoError(t, err)
	require.Equal(t, int64(9), values[f.job.UID].Groups["job"].Counts.Succeeded)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	event := newRemoteEvent(t, f.project, nil, "project.metadata_updated", "operator", spokeUID, time.Now().UnixMilli()+100, []byte(`{"diff":{"ordinary":{"from":"preserved","to":"from spoke"}}}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: f.project.ID, SpokeInstanceUID: spokeUID, BoundActor: "operator", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.NoError(t, err)
	project, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	values, err = db.DecodeCronTallies(project.Metadata)
	require.NoError(t, err)
	require.Equal(t, int64(9), values[f.job.UID].Groups["job"].Counts.Succeeded)
	require.Contains(t, string(project.Metadata), `"ordinary":"from spoke"`)
	_, err = f.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, `{"ordinary":"root current"}`, f.project.ID)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, f.project.ID))
	project, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	values, err = db.DecodeCronTallies(project.Metadata)
	require.NoError(t, err)
	require.Empty(t, values, "absence in current root namespace is authoritative too")
	_, err = f.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, `{"cron.tally.malformed":null}`, f.project.ID)
	require.NoError(t, err)
	require.Error(t, store.MaterializeFederatedProject(ctx, f.project.ID))
	return nil
}

func checkCronTallyImportAndEnable(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	f.seed(ctx, t, tallyLifecycleEntry(f, 9007199254740993))
	other := newCronTallyFixture(ctx, t, store)
	other.seed(ctx, t, tallyLifecycleEntry(other, 33))
	for _, scoped := range []bool{false, true} {
		filter := db.ExportFilter{IncludeDeleted: true}
		if scoped {
			filter.ProjectID = &f.project.ID
		}
		records, err := CollectImportRecords(ctx, store, filter)
		require.NoError(t, err)
		for _, newInstance := range []bool{false, true} {
			restored := backend.Open(t)
			closeRestored := sync.OnceFunc(func() { require.NoError(t, restored.Close()) })
			t.Cleanup(closeRestored)
			opts := db.ImportOptions{NewInstance: newInstance, MergeProject: scoped}
			require.NoError(t, restored.ImportReplay(ctx, records, opts))
			project, err := restored.ProjectByUID(ctx, f.project.UID)
			require.NoError(t, err)
			values, err := db.DecodeCronTallies(project.Metadata)
			require.NoError(t, err)
			require.Equal(t, int64(9007199254740993), values[f.job.UID].Groups["job"].Counts.Succeeded)
			_, err = restored.ProjectByUID(ctx, other.project.UID)
			if scoped {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			_, err = restored.EnableProjectFederation(ctx, project.ID, "operator")
			require.NoError(t, err)
			baseline, err := restored.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Features: db.CronEventFeature, Limit: 100})
			require.NoError(t, err)
			found := false
			for _, event := range baseline.Events {
				if event.Type == "project.federation_enabled" {
					require.Contains(t, string(event.Payload), `"succeeded":9007199254740993`)
					found = true
				}
			}
			require.True(t, found)
			closeRestored()
		}
		// Invalid project documents are refused before replace/merge starts.
		bad := append([]db.ImportRecord(nil), records...)
		for i, record := range bad {
			if row, ok := record.(*db.ProjectExport); ok && row.UID == f.project.UID {
				copied := *row
				copied.Metadata = jsontext.Value(`{"cron.tally.bad":null}`)
				bad[i] = &copied
			}
		}
		destination := backend.Open(t)
		closeDestination := sync.OnceFunc(func() { require.NoError(t, destination.Close()) })
		t.Cleanup(closeDestination)
		sentinel, err := destination.CreateProject(ctx, "untouched-project")
		require.NoError(t, err)
		require.Error(t, destination.ImportReplay(ctx, bad, db.ImportOptions{MergeProject: scoped}))
		preserved, err := destination.ProjectByID(ctx, sentinel.ID)
		require.NoError(t, err)
		require.Equal(t, sentinel, preserved)
		closeDestination()
	}
	// Reject corrupt stored values before enable emits a full metadata baseline.
	broken := newCronTallyFixture(ctx, t, store)
	_, err := broken.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{%q:null}`, db.CronTallyPrefix+broken.job.UID), broken.project.ID)
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, broken.project.ID, "operator")
	require.Error(t, err)
	project, err := store.ProjectByID(ctx, broken.project.ID)
	require.NoError(t, err)
	var raw map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(project.Metadata), &raw))
	require.Equal(t, "null", string(raw[db.CronTallyPrefix+broken.job.UID]))
	return nil
}

// Contract: live merge unions disjoint/equal histories; it never adds two
// potentially overlapping pruning histories or loses source evidence.
func checkCronTallyLiveMerge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, mode := range []string{"disjoint", "equal", "conflict", "issue-sync conflict", "kind collision", "pull-only target"} {
		source, target := newCronTallyFixture(ctx, t, store), newCronTallyFixture(ctx, t, store)
		if mode == "pull-only target" {
			_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: target.project.UID, Enabled: true, Actor: "operator"})
			require.NoError(t, err)
			_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.project.ID, TargetProjectID: target.project.ID, Actor: "operator"})
			require.ErrorIs(t, err, db.ErrProjectMergeFederationBinding)
			continue
		}
		a, b := tallyLifecycleEntry(source, 9007199254740993), tallyLifecycleEntry(target, 17)
		switch mode {
		case "equal", "conflict", "issue-sync conflict":
			b = a
			if mode != "equal" {
				b.Counts.Succeeded++
			}
		case "kind collision":
			b.UID = a.UID
			b.Kind = "workflow"
		}
		source.seed(ctx, t, a)
		target.seed(ctx, t, b)
		if mode == "issue-sync conflict" {
			// Both backends name the binding before reading tally evidence.
			_, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: target.project.ID, Provider: "github", SourceKey: "github:merge/target", RemoteID: "merge/target", DisplayName: "merge/target", Config: jsontext.Value(`{}`), IntervalSeconds: 60})
			require.NoError(t, err)
		}
		beforeSource, err := store.ProjectByID(ctx, source.project.ID)
		require.NoError(t, err)
		beforeTarget, err := store.ProjectByID(ctx, target.project.ID)
		require.NoError(t, err)
		result, err := store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.project.ID, TargetProjectID: target.project.ID, Actor: "operator"})
		if mode == "conflict" || mode == "issue-sync conflict" {
			require.ErrorIs(t, err, map[string]error{"conflict": db.ErrCronTallyConflict, "issue-sync conflict": db.ErrProjectMergeIssueSyncBinding}[mode])
			got, e := store.ProjectByID(ctx, source.project.ID)
			require.NoError(t, e)
			require.Equal(t, beforeSource, got)
			got, e = store.ProjectByID(ctx, target.project.ID)
			require.NoError(t, e)
			require.Equal(t, beforeTarget, got)
			continue
		}
		require.NoError(t, err)
		got, err := store.ProjectByID(ctx, target.project.ID)
		require.NoError(t, err)
		values, err := db.DecodeCronTallies(got.Metadata)
		require.NoError(t, err)
		require.Equal(t, a, values[a.UID].Groups[a.Kind])
		require.Equal(t, b, values[b.UID].Groups[b.Kind])
		require.Contains(t, string(got.Metadata), `"ordinary":"preserved"`)
		if mode == "equal" {
			require.Equal(t, beforeTarget.Revision, got.Revision)
			require.Zero(t, result.MetadataEvent.ID)
		} else {
			require.Equal(t, beforeTarget.Revision+1, got.Revision)
			require.Equal(t, "project.metadata_updated", result.MetadataEvent.Type)
			require.Equal(t, target.project.ID, result.MetadataEvent.ProjectID)
			require.Less(t, result.MetadataEvent.ID, result.Event.ID)
		}
		_, err = store.EnableProjectFederation(ctx, target.project.ID, "operator")
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(ctx, target.project.ID))
		after, err := store.ProjectByID(ctx, target.project.ID)
		require.NoError(t, err)
		require.JSONEq(t, string(got.Metadata), string(after.Metadata))
	}
	return nil
}

// Contract: only the owner's existing bounded adoption grant can transfer
// downstream tallies. Metadata-only chunks work through the terminal cursor;
// accepted hash-matching retries do not consume or add counts a second time.
func checkCronTallyAdoption(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, granted := range []bool{false, true} {
		f := newCronTallyFixture(ctx, t, store)
		root := tallyLifecycleEntry(f, 7)
		f.seed(ctx, t, root)
		_, err := store.EnableProjectFederation(ctx, f.project.ID, "operator")
		require.NoError(t, err)
		spoke, err := uid.New()
		require.NoError(t, err)
		created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &f.project.ID, SpokeInstanceUID: spoke, Actor: "operator", Capabilities: "push", AllowAdoptionSnapshotAuthors: granted})
		require.NoError(t, err)
		incoming := root
		incoming.UID, err = uid.New()
		require.NoError(t, err)
		incoming.Counts.Succeeded = 9007199254740993
		value, err := json.Marshal(db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": incoming}})
		require.NoError(t, err)
		events := []db.FederationIngestEvent{}
		baselineClock := time.Now().UnixMilli() + 100
		for i := range 3 {
			payload := fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, db.CronTallyPrefix+incoming.UID, value)
			event := newRemoteEvent(t, f.project, nil, "project.metadata_updated", "operator", spoke, baselineClock, []byte(payload))
			events = append(events, db.FederationIngestEvent{SourceEventID: 10 + int64(i), Event: event})
		}
		params := db.FederationIngestParams{ProjectID: f.project.ID, FederationEnrollmentID: created.Enrollment.ID, SpokeInstanceUID: spoke, BoundActor: "operator", EventFeatures: db.CronEventFeature, AllowSnapshotAuthorPreservation: true}
		if !granted {
			params.Events = events[:1]
			_, err = store.IngestFederationEvents(ctx, params)
			require.ErrorIs(t, err, db.ErrFederationIngestValidation)
			params.Events[0].Event.OriginInstanceUID = store.InstanceUID()
			params.Events[0].Event.ContentHash, err = db.RemoteEventContentHash(params.Events[0].Event)
			require.NoError(t, err)
			_, err = store.IngestFederationEvents(ctx, params)
			require.Error(t, err, "claiming root origin does not create authority")
			continue
		}
		params.AdoptionBaselineEndSourceEventID = 12
		for i := range 3 {
			params.Events = events[i : i+1]
			params.AdoptionBaseline = db.FederationAdoptionBaselineOpen
			if i == 2 {
				params.AdoptionBaseline = db.FederationAdoptionBaselineComplete
			}
			result, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err)
			require.Equal(t, 1, result.Accepted)
			project, err := store.ProjectByID(ctx, f.project.ID)
			require.NoError(t, err)
			values, err := db.DecodeCronTallies(project.Metadata)
			require.NoError(t, err)
			require.Equal(t, root, values[root.UID].Groups["job"])
			require.Equal(t, incoming, values[incoming.UID].Groups["job"])
			rootEvents := []db.Event{}
			for _, event := range result.Events {
				if event.OriginInstanceUID == store.InstanceUID() {
					rootEvents = append(rootEvents, event)
				}
			}
			if i == 0 {
				require.Len(t, rootEvents, 1)
				require.Equal(t, "project.metadata_updated", rootEvents[0].Type)
			} else {
				require.Empty(t, rootEvents, "equal history is not added")
			}

			// Continuation remains authorized after preservation of snapshot authors
			// has been disabled independently by an operator.
			_, err = f.q.ExecContext(ctx, `UPDATE federation_enrollments SET allow_adoption_snapshot_authors=0 WHERE id=$1`, created.Enrollment.ID)
			require.NoError(t, err)
		}
		result, err := store.IngestFederationEvents(ctx, params)
		require.NoError(t, err)
		require.Equal(t, 1, result.Duplicates)
		require.Empty(t, result.Events)
		params.Events = []db.FederationIngestEvent{events[0]}
		params.AdoptionBaseline = ""
		result, err = store.IngestFederationEvents(ctx, params)
		require.NoError(t, err)
		require.Equal(t, 1, result.Duplicates)
		fresh := events[0]
		fresh.Event.EventUID, err = uid.New()
		require.NoError(t, err)
		fresh.Event.ContentHash, err = db.RemoteEventContentHash(fresh.Event)
		require.NoError(t, err)
		params.Events = []db.FederationIngestEvent{fresh}
		_, err = store.IngestFederationEvents(ctx, params)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	}
	return nil
}

// Contract: a changed authorized adoption chunk publishes one root union,
// regardless of the number of incoming metadata records.
func checkCronTallyAdoptionChunk(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	hegel.Test(t, func(ht *hegel.T) {
		f := newCronTallyFixture(ctx, ht, store)
		root := tallyLifecycleEntry(f, 7)
		f.seed(ctx, ht, root)
		_, err := store.EnableProjectFederation(ctx, f.project.ID, "operator")
		require.NoError(ht, err)
		before, err := store.ProjectByID(ctx, f.project.ID)
		require.NoError(ht, err)
		spoke, err := uid.New()
		require.NoError(ht, err)
		created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &f.project.ID, SpokeInstanceUID: spoke, Actor: "operator", Capabilities: "push", AllowAdoptionSnapshotAuthors: true})
		require.NoError(ht, err)
		expected := []db.CronTallyEntry{root}
		events := []db.FederationIngestEvent{}
		boundary := time.Now().UnixMilli() + 100
		for i := range 2 {
			entry := root
			entry.UID, err = uid.New()
			require.NoError(ht, err)
			entry.Counts.Succeeded = hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64))
			expected = append(expected, entry)
			value, err := json.Marshal(db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": entry}})
			require.NoError(ht, err)
			payload := fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, db.CronTallyPrefix+entry.UID, value)
			event := newRemoteEvent(t, f.project, nil, "project.metadata_updated", "operator", spoke, boundary, []byte(payload))
			events = append(events, db.FederationIngestEvent{SourceEventID: int64(i + 1), Event: event})
		}
		params := db.FederationIngestParams{ProjectID: f.project.ID, FederationEnrollmentID: created.Enrollment.ID, SpokeInstanceUID: spoke, BoundActor: "operator", EventFeatures: db.CronEventFeature, AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineComplete, AdoptionBaselineEndSourceEventID: 2, Events: events}
		result, err := store.IngestFederationEvents(ctx, params)
		require.NoError(ht, err)
		require.Equal(ht, 2, result.Accepted)
		rootEvents := []db.Event{}
		for _, event := range result.Events {
			if event.OriginInstanceUID == store.InstanceUID() && event.Type == "project.metadata_updated" {
				rootEvents = append(rootEvents, event)
			}
		}
		require.Len(ht, rootEvents, 1, "one root union event per changed chunk")
		project, err := store.ProjectByID(ctx, f.project.ID)
		require.NoError(ht, err)
		require.Equal(ht, before.Revision+1, project.Revision)
		values, err := db.DecodeCronTallies(project.Metadata)
		require.NoError(ht, err)
		require.Len(ht, values, len(expected))
		for _, entry := range expected {
			require.Equal(ht, entry, values[entry.UID].Groups[entry.Kind])
		}
		retry, err := store.IngestFederationEvents(ctx, params)
		require.NoError(ht, err)
		require.Equal(ht, 2, retry.Duplicates)
		require.Empty(ht, retry.Events)
	}, hegel.WithTestCases(30))
	return nil
}

func checkCronTallyEqualSubsetReplay(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, mode := range []string{"subset", "deletion", "changed-union", "whole-equal"} {
		t.Run(mode, func(t *testing.T) {
			f := newCronTallyFixture(ctx, t, store)
			job := tallyLifecycleEntry(f, 7)
			workflow := job
			workflow.Kind = "workflow"
			workflow.Counts.Succeeded = 11
			full := db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": job, "workflow": workflow}}
			if mode == "changed-union" {
				delete(full.Groups, "workflow")
			}
			raw, err := json.Marshal(map[string]db.CronTallyValue{db.CronTallyPrefix + job.UID: full}, json.Deterministic(true))
			require.NoError(t, err)
			_, err = f.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), f.project.ID)
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(ctx, f.project.ID, "operator")
			require.NoError(t, err)
			spoke, err := uid.New()
			require.NoError(t, err)
			grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &f.project.ID, SpokeInstanceUID: spoke, Actor: "operator", Capabilities: "push", AllowAdoptionSnapshotAuthors: true})
			require.NoError(t, err)
			subset, err := json.Marshal(db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": job}})
			require.NoError(t, err)
			payload := fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, db.CronTallyPrefix+job.UID, subset)
			eventType := "project.metadata_updated"
			switch mode {
			case "deletion":
				payload = fmt.Sprintf(`{"diff":{%q:{"from":%s,"to":null}}}`, db.CronTallyPrefix+job.UID, subset)
			case "changed-union":
				incoming, encodeErr := json.Marshal(db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"workflow": workflow}})
				require.NoError(t, encodeErr)
				payload = fmt.Sprintf(`{"diff":{%q:{"from":%s,"to":%s}}}`, db.CronTallyPrefix+job.UID, subset, incoming)
			case "whole-equal":
				fullValue, encodeErr := json.Marshal(full)
				require.NoError(t, encodeErr)
				payload = fmt.Sprintf(`{"diff":{%q:{"from":null,"to":%s}}}`, db.CronTallyPrefix+job.UID, fullValue)
			}
			event := newRemoteEvent(t, f.project, nil, eventType, "operator", spoke, time.Now().UnixMilli()+100, []byte(payload))
			params := db.FederationIngestParams{ProjectID: f.project.ID, FederationEnrollmentID: grant.Enrollment.ID, SpokeInstanceUID: spoke, BoundActor: "operator", EventFeatures: db.CronEventFeature, AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineComplete, AdoptionBaselineEndSourceEventID: 1, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
			result, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err)
			require.Equal(t, 1, result.Accepted)
			project, err := store.ProjectByID(ctx, f.project.ID)
			require.NoError(t, err)
			current, err := db.DecodeCronTallies(project.Metadata)
			require.NoError(t, err)
			require.Len(t, current[job.UID].Groups, 2, "root retains the union")
			read, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: f.project.ID, Features: db.CronEventFeature, Limit: 100})
			require.NoError(t, err)
			foldEvents := []db.FoldEvent{}
			for _, e := range read.Events {
				foldEvents = append(foldEvents, db.FoldEvent{UID: e.UID, ProjectUID: f.project.UID, OriginInstanceUID: e.OriginInstanceUID, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, Type: e.Type, Actor: e.Actor, Payload: jsontext.Value(e.Payload)})
			}
			projection := db.FoldEvents(foldEvents)
			replay, err := db.DecodeCronTallies(db.JSONBlob(projection.ProjectMetadata[f.project.UID]))
			require.NoError(t, err)
			require.Equal(t, current, replay, "root-approved pull history must reproduce current authoritative tally union")
			// The unchanged issue-metadata fold is the exact recursive fold used
			// for project metadata before tally support. Replay real store output.
			legacyEvents := []db.FoldEvent{}
			for _, e := range foldEvents {
				e.IssueUID = "legacy-peer"
				switch e.Type {
				case "project.metadata_updated":
					e.Type = "issue.metadata_updated"
				case "project.federation_enabled":
					e.Type = "issue.snapshot"
					e.Payload, err = json.Marshal(map[string]any{"uid": e.IssueUID, "metadata": db.PayloadMap(e.Payload)["metadata"]})
					require.NoError(t, err)
				default:
					continue
				}
				legacyEvents = append(legacyEvents, e)
			}
			require.JSONEq(t, string(project.Metadata), string(db.FoldEvents(legacyEvents).IssueMetadata["legacy-peer"]), "pre-tally peers must recover the complete root tally")
			correctionCount := 0
			for _, emitted := range result.Events {
				if emitted.OriginInstanceUID == store.InstanceUID() && emitted.Type == "project.metadata_updated" {
					correctionCount++
				}
			}
			want := 1
			if mode == "whole-equal" {
				want = 0
			}
			require.Equal(t, want, correctionCount)
			retry, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err)
			require.Equal(t, 1, retry.Duplicates)
			require.Empty(t, retry.Events)
		})
	}
	return nil
}
