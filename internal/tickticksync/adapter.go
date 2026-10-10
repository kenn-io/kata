package tickticksync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// Adapter stages complete validated observations before guarded native imports.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
	now     func() time.Time
}

// NewAdapter connects scoped observations to durable shared sync storage.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter {
	return &Adapter{store: store, fetcher: fetcher, now: time.Now}
}

// Provider names this adapter for binding selection.
func (*Adapter) Provider() string { return "ticktick" }

// InitialPhase names the first visible progress phase.
func (*Adapter) InitialPhase() string { return "project" }
func bindingConfig(b db.IssueSyncBinding) (Config, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return c, err
	}
	if b.Provider != "ticktick" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return c, fmt.Errorf("TickTick binding identity does not match config")
	}
	return c, nil
}

// Prepare stages validated source versions before any guarded import chunk.
func (a *Adapter) Prepare(ctx context.Context, b db.IssueSyncBinding, started time.Time) (issuesync.Prepared, error) {
	p := issuesync.Prepared{Binding: b}
	if a.store == nil || a.fetcher == nil {
		return p, fmt.Errorf("TickTick adapter requires store and fetcher")
	}
	c, err := bindingConfig(b)
	if err != nil {
		return p, err
	}
	cp, err := DecodeCheckpoint(b.Config)
	if err != nil {
		return p, err
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return p, err
	}
	issuesync.ReportProgress(ctx, "tasks", 0, 0)
	data, err := session.Data(ctx)
	if err != nil {
		return p, err
	}
	// Date content by when it was fetched. In two-way mode the status pass runs
	// first and can take minutes after the claim started.
	observedAt := a.now()
	visible := map[string]bool{}
	for _, t := range data.Tasks {
		visible[t.ID] = true
	}
	missing, closedCandidates, err := a.missingTaskWindow(ctx, b, visible, &cp)
	if err != nil {
		return p, err
	}
	recovered, err := recoverMissingTasks(ctx, session, a.now, missing, closedCandidates, &cp, &data)
	if err != nil {
		return p, err
	}
	if err = a.seedReturningTasks(ctx, b, data, &cp); err != nil {
		return p, err
	}
	batch, next, err := BuildImportBatch(c, data, cp, observedAt)
	if err != nil {
		return p, err
	}
	staged := stagedCheckpoint(cp, next, recovered)
	stagedRaw, err := WithCheckpoint(b.Config, staged)
	if err != nil {
		return p, err
	}
	finalRaw, err := WithCheckpoint(b.Config, next)
	if err != nil {
		return p, err
	}
	name := strings.TrimSpace(data.Project.Name)
	if name == "" {
		name = "TickTick project " + c.ProjectID
	}
	var oldFields, newFields map[string]any
	_ = json.Unmarshal(b.Config, &oldFields)
	_ = json.Unmarshal(stagedRaw, &newFields)
	oldCanonical, _ := json.Marshal(oldFields, json.Deterministic(true))
	newCanonical, _ := json.Marshal(newFields, json.Deterministic(true))
	if string(oldCanonical) != string(newCanonical) || name != b.DisplayName {
		b, err = a.store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: b.ID, DisplayName: name, Config: stagedRaw, StartedAt: &started, BindingUpdatedAt: new(b.UpdatedAt), ReplaceProviderCheckpoint: true})
		if err != nil {
			return p, err
		}
		p.Binding = b
	}
	p.Batch = batch
	if len(recovered) > 0 {
		finalizeBinding := b
		p.Finalize = func(finalizeCtx context.Context) (db.IssueSyncBinding, error) {
			return a.store.RefreshIssueSyncBinding(finalizeCtx, db.IssueSyncBindingUpdateParams{
				BindingID: finalizeBinding.ID, DisplayName: name, Config: finalRaw, StartedAt: &started,
				BindingUpdatedAt: new(finalizeBinding.UpdatedAt), ReplaceProviderCheckpoint: true,
			})
		}
	}
	issuesync.ReportProgress(ctx, "content", len(batch.Items), len(batch.Items))
	return p, nil
}

// missingTaskWindow selects this run's bounded rotation of missing tasks to
// read, prunes checkpoint entries no longer tracked, and advances the rotation.
func (a *Adapter) missingTaskWindow(ctx context.Context, b db.IssueSyncBinding, visible map[string]bool, cp *Checkpoint) ([]string, map[string]bool, error) {
	openMissing, err := a.openMissingTasks(ctx, b, visible)
	if err != nil {
		return nil, nil, err
	}
	closedCandidates, err := a.closedMissingTaskCandidates(ctx, b, visible, *cp)
	if err != nil {
		return nil, nil, err
	}
	unimported, err := a.unimportedMissingTasks(ctx, b, visible, *cp)
	if err != nil {
		return nil, nil, err
	}
	tracked := map[string]bool{}
	for _, id := range slices.Concat(openMissing, unimported) {
		tracked[id] = true
	}
	for id := range closedCandidates {
		tracked[id] = true
	}
	allMissing := slices.Sorted(maps.Keys(tracked))
	maps.DeleteFunc(cp.Versions, func(id string, _ TaskVersion) bool { return !visible[id] && !tracked[id] })
	index := sort.SearchStrings(allMissing, cp.MissingAfter)
	if index < len(allMissing) && allMissing[index] == cp.MissingAfter {
		index++
	}
	if index >= len(allMissing) {
		index = 0
	}
	end := min(index+missingTaskLookupLimit, len(allMissing))
	cp.MissingAfter = ""
	if end < len(allMissing) && end > 0 {
		cp.MissingAfter = allMissing[end-1]
	}
	return allMissing[index:end], closedCandidates, nil
}

// recoverMissingTasks reads the selected missing tasks and appends the ones
// that still need an import to data. Tasks that left the project are forgotten.
func recoverMissingTasks(ctx context.Context, session Session, now func() time.Time, missing []string, closedCandidates map[string]bool, cp *Checkpoint, data *ProjectData) (map[string]bool, error) {
	issuesync.ReportProgress(ctx, "missing-tasks", 0, len(missing))
	recovered := map[string]bool{}
	for n, id := range missing {
		issuesync.ReportProgress(ctx, "missing-tasks", n+1, len(missing))
		t, err := session.Task(ctx, id)
		if se, ok := errors.AsType[*issuesync.StatusError](err); ok && se.HTTPStatus == 404 || errors.Is(err, errTaskOutsideProject) {
			delete(cp.Versions, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		if t.Status == nil {
			return nil, blocked("TickTick task status is missing")
		}
		if *t.Status == -1 || t.Kind == "NOTE" {
			delete(cp.Versions, id)
			continue
		}
		if closedCandidates[id] && *t.Status != 2 && !cp.Versions[id].PendingRecovery {
			continue
		}
		// Date the task when it was read; the reads run after the list fetch.
		t.observedAt = now()
		data.Tasks = append(data.Tasks, t)
		recovered[id] = true
	}
	return recovered, nil
}

// stagedCheckpoint is the checkpoint saved before import. Recovered tasks stay
// marked pending until Finalize saves next after the import commits, so a
// failed import reads them again.
func stagedCheckpoint(cp, next Checkpoint, recovered map[string]bool) Checkpoint {
	staged := Checkpoint{Versions: maps.Clone(next.Versions), MissingAfter: next.MissingAfter}
	for id := range recovered {
		observed, ok := staged.Versions[id]
		if !ok {
			if previous, had := cp.Versions[id]; had {
				staged.Versions[id] = previous
			}
			continue
		}
		observed.PendingRecovery = true
		staged.Versions[id] = observed
	}
	return staged
}

// seedReturningTasks gives a mapped task with no checkpoint entry, such as one
// that sync stopped tracking, its saved source version as a baseline. Unchanged
// content then cannot replace newer local edits.
func (a *Adapter) seedReturningTasks(ctx context.Context, b db.IssueSyncBinding, data ProjectData, cp *Checkpoint) error {
	returning := map[string]Task{}
	for _, t := range data.Tasks {
		if _, tracked := cp.Versions[t.ID]; tracked || t.Status == nil || *t.Status == -1 || t.Kind == "NOTE" {
			continue
		}
		returning[t.ID] = t
	}
	if len(returning) == 0 {
		return nil
	}
	mappings, err := a.store.ImportMappingsByProjectSource(ctx, b.ProjectID, b.SourceKey)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.ObjectType != "issue" || m.SourceUpdatedAt == nil {
			continue
		}
		id, err := mappingTaskID(m.ExternalID)
		if err != nil {
			return err
		}
		task, ok := returning[id]
		if !ok {
			continue
		}
		hash, err := taskContentHash(task)
		if err != nil {
			return err
		}
		saved := m.SourceUpdatedAt.UTC().Truncate(time.Millisecond)
		cp.Versions[id] = TaskVersion{Hash: hash, FirstSeen: saved, Version: saved}
	}
	return nil
}

// unimportedMissingTasks lists checkpointed tasks absent from the current
// collection that have no issue mapping yet. A failed import leaves such an
// entry; if the task completed before the retry, only an individual read can
// still import it.
func (a *Adapter) unimportedMissingTasks(ctx context.Context, b db.IssueSyncBinding, visible map[string]bool, cp Checkpoint) ([]string, error) {
	absent := map[string]bool{}
	for id := range cp.Versions {
		if !visible[id] {
			absent[id] = true
		}
	}
	if len(absent) == 0 {
		return nil, nil
	}
	mappings, err := a.store.ImportMappingsByProjectSource(ctx, b.ProjectID, b.SourceKey)
	if err != nil {
		return nil, err
	}
	for _, m := range mappings {
		if m.ObjectType != "issue" || m.IssueID == nil {
			continue
		}
		id, err := mappingTaskID(m.ExternalID)
		if err != nil {
			return nil, err
		}
		delete(absent, id)
	}
	return slices.Sorted(maps.Keys(absent)), nil
}

// openMissingTasks lists mapped tasks absent from the current collection whose
// Kata issue is still open. The collection omits completed tasks, so only these
// can still change status; closed history is not read again. A task that
// reappears in the collection is imported again as a new observation.
func (a *Adapter) openMissingTasks(ctx context.Context, b db.IssueSyncBinding, visible map[string]bool) ([]string, error) {
	mappings, err := a.store.ImportMappingsByProjectSource(ctx, b.ProjectID, b.SourceKey)
	if err != nil {
		return nil, err
	}
	taskByIssue := map[int64]string{}
	issueIDs := []int64{}
	for _, m := range mappings {
		if m.ObjectType != "issue" || m.IssueID == nil {
			continue
		}
		id, err := mappingTaskID(m.ExternalID)
		if err != nil {
			return nil, err
		}
		if !visible[id] {
			taskByIssue[*m.IssueID] = id
			issueIDs = append(issueIDs, *m.IssueID)
		}
	}
	if len(issueIDs) == 0 {
		return []string{}, nil
	}
	open, err := a.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: b.ProjectID, Status: "open", AllowedIssueIDs: issueIDs})
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0, len(open))
	for _, issue := range open {
		id, ok := taskByIssue[issue.ID]
		if !ok {
			return nil, fmt.Errorf("TickTick open issue has no task mapping")
		}
		missing = append(missing, id)
	}
	sort.Strings(missing)
	return missing, nil
}

// closedMissingTaskCandidates keeps locally closed tasks that TickTick last
// showed open eligible for bounded reads. The read records the completion
// before sync stops tracking the task, so a later TickTick reopen is a real
// status change, and it imports the task's final content.
func (a *Adapter) closedMissingTaskCandidates(ctx context.Context, b db.IssueSyncBinding, visible map[string]bool, checkpoint Checkpoint) (map[string]bool, error) {
	mappings, err := a.store.ImportMappingsByProjectSource(ctx, b.ProjectID, b.SourceKey)
	if err != nil {
		return nil, err
	}
	taskByIssue := map[int64]string{}
	issueIDs := []int64{}
	for _, mapping := range mappings {
		if mapping.ObjectType != "issue" || mapping.IssueID == nil {
			continue
		}
		id, err := mappingTaskID(mapping.ExternalID)
		if err != nil {
			return nil, err
		}
		if visible[id] {
			continue
		}
		version, ok := checkpoint.Versions[id]
		if !ok {
			continue
		}
		if !version.PendingRecovery && version.Status != 0 {
			continue
		}
		taskByIssue[*mapping.IssueID] = id
		issueIDs = append(issueIDs, *mapping.IssueID)
	}
	if len(issueIDs) == 0 {
		return map[string]bool{}, nil
	}
	closed, err := a.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: b.ProjectID, Status: "closed", AllowedIssueIDs: issueIDs})
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]bool, len(closed))
	for _, issue := range closed {
		if task, ok := taskByIssue[issue.ID]; ok {
			candidates[task] = true
		}
	}
	return candidates, nil
}

func mappingTaskID(external string) (string, error) {
	if !strings.HasPrefix(external, "task:") {
		return "", blocked("invalid TickTick mapping identity")
	}
	id := strings.TrimPrefix(external, "task:")
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return id, nil
}

// OpenStatus captures one credential snapshot for the independent status pass.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	c, err := bindingConfig(b)
	if err != nil {
		return nil, err
	}
	if a.fetcher == nil {
		return nil, fmt.Errorf("TickTick adapter requires fetcher")
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	status, ok := session.(StatusSession)
	if !ok {
		return nil, blocked("TickTick source does not support status synchronization")
	}
	if _, err = session.Project(ctx); err != nil {
		return nil, statusReadError(err)
	}
	return &statusRun{session: status}, nil
}

type statusRun struct{ session StatusSession }

func (r *statusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	id, err := mappingTaskID(m.Mapping.ExternalID)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return r.session.ReadStatus(ctx, id)
}
func (r *statusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	id, err := mappingTaskID(m.Mapping.ExternalID)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return r.session.WriteStatus(ctx, id, desired, admit)
}

// RunnerConfig reuses daemon scheduling, event delivery, and drain authority.
type RunnerConfig struct {
	Store          db.Storage
	Fetcher        Fetcher
	Progress       *issuesync.ProgressTracker
	Clock          func() time.Time
	Logger         *slog.Logger
	Interval       time.Duration
	Wake           <-chan struct{}
	EventSink      func(context.Context, int64, []db.Event) error
	EventSinkFrom  func(context.Context, int64, []db.Event, activity.Admission) error
	DrainAdmission activity.WaitableAdmission
}

// NewRunner uses shared scheduling, guarded imports, and status intent delivery.
func NewRunner(c RunnerConfig) *issuesync.Runner {
	return issuesync.NewRunner(issuesync.RunnerConfig{Store: c.Store, Adapter: newRunnerAdapter(c), Progress: c.Progress, Clock: c.Clock, Logger: c.Logger, Interval: c.Interval, Wake: c.Wake, EventSink: c.EventSink, EventSinkFrom: c.EventSinkFrom, DrainAdmission: c.DrainAdmission, RunTimeout: 20 * time.Minute, StaleLockTTL: 30 * time.Minute, InitialBatchSize: 5000})
}

func newRunnerAdapter(c RunnerConfig) *Adapter {
	adapter := NewAdapter(c.Store, c.Fetcher)
	if c.Clock != nil {
		adapter.now = c.Clock
	}
	return adapter
}
