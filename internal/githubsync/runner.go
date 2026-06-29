package githubsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

const (
	// DefaultStaleLockTTL is the recovery horizon for an abandoned sync claim.
	DefaultStaleLockTTL = 30 * time.Minute
	// DefaultInitialBatchSize limits the number of imported issues per batch.
	DefaultInitialBatchSize = 5000
)

const (
	runnerDueLimit       = 100
	runnerCleanupTimeout = 10 * time.Second
)

// Runner runs durable GitHub sync bindings through the import pipeline.
type Runner struct {
	config RunnerConfig
}

// RunnerConfig configures a GitHub sync runner.
type RunnerConfig struct {
	Store            db.Storage
	Fetcher          Fetcher
	Clock            func() time.Time
	EventSink        func(context.Context, int64, []db.Event) error
	Logger           *slog.Logger
	Interval         time.Duration
	Wake             <-chan struct{}
	StaleLockTTL     time.Duration
	InitialBatchSize int
}

// RunResult summarizes one completed sync attempt.
type RunResult struct {
	Binding db.IssueSyncBinding
	Status  db.IssueSyncStatus
	Import  db.ImportBatchResult
}

// NewRunner returns a runner with defaults applied.
func NewRunner(config RunnerConfig) *Runner {
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.StaleLockTTL <= 0 {
		config.StaleLockTTL = DefaultStaleLockTTL
	}
	if config.InitialBatchSize <= 0 {
		config.InitialBatchSize = DefaultInitialBatchSize
	}
	return &Runner{config: config}
}

// RunOnce syncs one binding if it can claim the binding lock.
func (r *Runner) RunOnce(ctx context.Context, bindingID int64) (RunResult, error) {
	if err := r.validate(); err != nil {
		return RunResult{}, err
	}
	syncStartedAt := r.now()
	binding, claimed, err := r.config.Store.ClaimIssueSyncBinding(ctx, bindingID, "github", syncStartedAt, syncStartedAt.Add(-r.staleLockTTL()))
	if err != nil {
		return RunResult{}, err
	}
	if !claimed {
		return RunResult{Binding: binding}, db.ErrIssueSyncAlreadyRunning
	}

	result, err := r.runClaimed(ctx, binding, syncStartedAt)
	if err != nil {
		return result, err
	}
	return result, nil
}

// Run processes currently due bindings serially, then repeats on Interval when
// configured. With Interval <= 0 it performs one due scan and returns.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.config.Interval <= 0 {
		return r.runDue(ctx)
	}
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	for {
		if err := r.runDue(ctx); err != nil {
			if isContextError(err) {
				return err
			}
			r.config.Logger.Warn("github sync due pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-r.config.Wake:
		}
	}
}

func (r *Runner) runDue(ctx context.Context) error {
	now := r.now()
	bindings, err := r.config.Store.ListDueIssueSyncBindings(ctx, "github", now, now.Add(-r.staleLockTTL()), runnerDueLimit)
	if err != nil {
		return err
	}
	var errs []error
	for _, binding := range bindings {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := r.RunOnce(ctx, binding.ID); err != nil {
			if errorsIsAlreadyRunning(err) {
				continue
			}
			if isContextError(err) {
				return err
			}
			r.config.Logger.Warn("github sync binding failed", "binding_id", binding.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) runClaimed(ctx context.Context, binding db.IssueSyncBinding, syncStartedAt time.Time) (RunResult, error) {
	if binding.Provider != "github" {
		err := fmt.Errorf("github sync runner cannot run provider %q", binding.Provider)
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	ghConfig, err := DecodeConfig(binding.Config)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	reconcileLegacyTitles := ghConfig.TitlePrefix == nil
	activeBinding := ghConfig.Binding()
	fetcher, err := r.fetcherForBinding(ctx, activeBinding)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	repo, err := fetcher.Repository(ctx, ghConfig.Host, ghConfig.Owner, ghConfig.Repo)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	if strings.TrimSpace(repo.NodeID) != binding.RemoteID {
		err := fmt.Errorf("github sync repository node mismatch: binding has %q, fetch returned %q", binding.RemoteID, repo.NodeID)
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	binding, ghConfig, err = r.refreshRepository(ctx, binding, ghConfig, repo)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	if refreshedBinding := ghConfig.Binding(); refreshedBinding != activeBinding {
		fetcher, err = r.fetcherForBinding(ctx, refreshedBinding)
		if err != nil {
			return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
		}
	}

	parentData, err := fetcher.ParentData(ctx, ghConfig.Binding())
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	parentLinkBackfill := ghConfig.NeedsParentLinkBackfill() && parentData.Authoritative

	since := syncSince(binding.LastCursorAt)
	if reconcileLegacyTitles || parentLinkBackfill {
		since = nil
	}
	issues, err := fetcher.Issues(ctx, ghConfig.Binding(), since)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	comments, err := r.fetchComments(ctx, fetcher, ghConfig, issues)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}

	batch := BuildImportBatchWithConfig(binding.SourceKey, ghConfig, issues, comments, parentData, syncStartedAt)
	batch.ProjectID = binding.ProjectID
	batch.PreserveLocalParentConflicts = true
	if parentData.Authoritative {
		batch.ReconcileLinkTypesForUnchanged = map[string]bool{"parent": true}
		batch.Items, err = r.appendScannedParentReconcileItems(ctx, batch, parentData)
		if err != nil {
			return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
		}
	}
	if parentLinkBackfill {
		batch.ReconcileLinkTypesForUnchanged = map[string]bool{"parent": true}
	}
	batch.Items, err = r.filterUnresolvableParentLinks(ctx, batch)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, db.ImportBatchResult{})
	}
	batch.Items = orderImportItemsForLinkTargets(batch.Items)
	batch.IssueSyncGuard = &db.IssueSyncImportGuard{
		BindingID: binding.ID,
		Provider:  "github",
		StartedAt: syncStartedAt,
	}
	importResult, err := r.importChunks(ctx, binding, batch)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, importResult)
	}
	cleanupCtx, cleanupCancel := r.cleanupContext()
	defer cleanupCancel()
	if parentLinkBackfill {
		backfilledConfig := ghConfig.WithParentLinksBackfilled()
		configJSON, err := EncodeConfig(backfilledConfig)
		if err != nil {
			return r.recordError(ctx, binding, syncStartedAt, err, importResult)
		}
		refreshedBinding, err := r.config.Store.RefreshIssueSyncBinding(cleanupCtx, db.IssueSyncBindingUpdateParams{
			BindingID:   binding.ID,
			DisplayName: backfilledConfig.DisplayName(),
			Config:      configJSON,
		})
		if err != nil {
			return r.recordError(ctx, binding, syncStartedAt, err, importResult)
		}
		binding = refreshedBinding
		ghConfig = backfilledConfig
	}
	status, err := r.config.Store.RecordIssueSyncSuccess(cleanupCtx, db.IssueSyncSuccessParams{
		BindingID:     binding.ID,
		StartedAt:     syncStartedAt,
		At:            r.now(),
		CursorAt:      syncStartedAt,
		LastCreated:   importResult.Created,
		LastUpdated:   importResult.Updated,
		LastUnchanged: importResult.Unchanged,
		LastComments:  importResult.Comments,
	})
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, err, importResult)
	}
	binding.LastCursorAt = &syncStartedAt
	return RunResult{Binding: binding, Status: status, Import: importResult}, nil
}

func (r *Runner) refreshRepository(ctx context.Context, binding db.IssueSyncBinding, ghConfig Config, repo Repository) (db.IssueSyncBinding, Config, error) {
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return binding, ghConfig, fmt.Errorf("github repository full_name %q is invalid", repo.FullName)
	}
	refreshedConfig := Config{
		Host:               ghConfig.Host,
		Owner:              owner,
		Repo:               name,
		RepoID:             repo.ID,
		TitlePrefix:        ghConfig.TitlePrefix,
		ParentLinksVersion: ghConfig.ParentLinksVersion,
	}
	configJSON, err := EncodeConfig(refreshedConfig)
	if err != nil {
		return binding, ghConfig, err
	}
	if binding.DisplayName == refreshedConfig.DisplayName() && string(binding.Config) == string(configJSON) {
		return binding, refreshedConfig, nil
	}
	refreshed, err := r.config.Store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{
		BindingID:   binding.ID,
		DisplayName: refreshedConfig.DisplayName(),
		Config:      configJSON,
	})
	return refreshed, refreshedConfig, err
}

func (r *Runner) fetcherForBinding(ctx context.Context, binding Binding) (Fetcher, error) {
	if sessionFetcher, ok := r.config.Fetcher.(BindingSessionFetcher); ok {
		return sessionFetcher.ForBinding(ctx, binding)
	}
	return r.config.Fetcher, nil
}

func (r *Runner) fetchComments(ctx context.Context, fetcher Fetcher, ghConfig Config, issues []Issue) (map[int][]Comment, error) {
	out := make(map[int][]Comment)
	fetchBinding := ghConfig.Binding()
	for _, issue := range issues {
		if IsPullRequestIssue(issue) {
			continue
		}
		if issue.Comments == 0 {
			continue
		}
		comments, err := fetcher.Comments(ctx, fetchBinding, issue.Number)
		if err != nil {
			return nil, err
		}
		out[issue.Number] = comments
	}
	return out, nil
}

func (r *Runner) appendScannedParentReconcileItems(ctx context.Context, batch db.ImportBatchParams, parentData ParentData) ([]db.ImportItem, error) {
	if !parentData.Authoritative || len(parentData.ChildIDByNumber) == 0 {
		return batch.Items, nil
	}
	present := make(map[string]struct{}, len(batch.Items))
	for _, item := range batch.Items {
		present[item.ExternalID] = struct{}{}
	}
	numbers := make([]int, 0, len(parentData.ChildIDByNumber))
	for number := range parentData.ChildIDByNumber {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	items := append([]db.ImportItem(nil), batch.Items...)
	for _, number := range numbers {
		childID := parentData.ChildIDByNumber[number]
		if childID == 0 {
			continue
		}
		childExternalID := fmt.Sprintf("issue-id:%d", childID)
		if _, ok := present[childExternalID]; ok {
			continue
		}
		mapping, err := r.config.Store.ImportMappingBySource(ctx, batch.ProjectID, batch.Source, "issue", childExternalID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				r.config.Logger.Warn("github sync skipped parent reconciliation for unmapped scanned child",
					"source", batch.Source,
					"child_external_id", childExternalID,
					"child_number", number,
				)
				continue
			}
			return nil, fmt.Errorf("lookup github scanned child %q: %w", childExternalID, err)
		}
		if mapping.IssueID == nil {
			return nil, fmt.Errorf("%w: github scanned child mapping %q missing issue_id", db.ErrNotFound, childExternalID)
		}
		issue, err := r.config.Store.IssueByID(ctx, *mapping.IssueID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				r.config.Logger.Warn("github sync skipped parent reconciliation for missing mapped child",
					"source", batch.Source,
					"child_external_id", childExternalID,
					"child_number", number,
				)
				continue
			}
			return nil, fmt.Errorf("lookup github scanned child issue %q: %w", childExternalID, err)
		}
		item := db.ImportItem{
			ExternalID: childExternalID,
			Title:      issue.Title,
			Body:       issue.Body,
			Author:     issue.Author,
			Owner:      issue.Owner,
			Status:     issue.Status,
			CreatedAt:  issue.CreatedAt,
			UpdatedAt:  issue.UpdatedAt,
			ClosedAt:   issue.ClosedAt,
			LinkTypesAuthoritative: map[string]bool{
				"parent": parentData.ChildScanned(number),
			},
		}
		if issue.ClosedReason != nil {
			reason := *issue.ClosedReason
			item.ClosedReason = &reason
		}
		if parentID, ok := parentData.ParentID(number); ok {
			item.Links = append(item.Links, db.ImportLink{
				Type:             "parent",
				TargetExternalID: fmt.Sprintf("issue-id:%d", parentID),
			})
		}
		items = append(items, item)
		present[childExternalID] = struct{}{}
	}
	return items, nil
}

func (r *Runner) filterUnresolvableParentLinks(ctx context.Context, batch db.ImportBatchParams) ([]db.ImportItem, error) {
	if len(batch.Items) == 0 {
		return batch.Items, nil
	}
	inBatch := make(map[string]struct{}, len(batch.Items))
	for _, item := range batch.Items {
		inBatch[item.ExternalID] = struct{}{}
	}
	items := append([]db.ImportItem(nil), batch.Items...)
	for i := range items {
		if len(items[i].Links) == 0 {
			continue
		}
		links := items[i].Links[:0]
		for _, link := range items[i].Links {
			if link.Type != "parent" {
				links = append(links, link)
				continue
			}
			if _, ok := inBatch[link.TargetExternalID]; ok {
				links = append(links, link)
				continue
			}
			if _, err := r.config.Store.ImportMappingBySource(ctx, batch.ProjectID, batch.Source, "issue", link.TargetExternalID); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					r.config.Logger.Warn("github sync skipped unresolved parent link",
						"source", batch.Source,
						"child_external_id", items[i].ExternalID,
						"target_external_id", link.TargetExternalID,
					)
					markParentLinkNonAuthoritative(&items[i])
					continue
				}
				return nil, fmt.Errorf("lookup github parent link target %q: %w", link.TargetExternalID, err)
			}
			links = append(links, link)
		}
		items[i].Links = links
	}
	return items, nil
}

func markParentLinkNonAuthoritative(item *db.ImportItem) {
	if item.LinkTypesAuthoritative == nil {
		item.LinkTypesAuthoritative = map[string]bool{}
	}
	item.LinkTypesAuthoritative["parent"] = false
}

func (r *Runner) importChunks(ctx context.Context, binding db.IssueSyncBinding, batch db.ImportBatchParams) (db.ImportBatchResult, error) {
	chunkSize := r.initialBatchSize()
	aggregate := db.ImportBatchResult{Source: batch.Source, Errors: []string{}}
	if len(batch.Items) == 0 {
		res, events, err := r.config.Store.ImportBatch(ctx, batch)
		if err != nil {
			return aggregate, err
		}
		mergeImportResult(&aggregate, res)
		r.emitEvents(ctx, binding.ProjectID, events)
		return aggregate, nil
	}
	for start := 0; start < len(batch.Items); start += chunkSize {
		end := start + chunkSize
		if end > len(batch.Items) {
			end = len(batch.Items)
		}
		chunk := batch
		chunk.Items = append([]db.ImportItem(nil), batch.Items[start:end]...)
		res, events, err := r.config.Store.ImportBatch(ctx, chunk)
		if err != nil {
			return aggregate, err
		}
		mergeImportResult(&aggregate, res)
		r.emitEvents(ctx, binding.ProjectID, events)
	}
	return aggregate, nil
}

func (r *Runner) emitEvents(ctx context.Context, projectID int64, events []db.Event) {
	if r.config.EventSink == nil || len(events) == 0 {
		return
	}
	if err := r.config.EventSink(ctx, projectID, events); err != nil {
		r.config.Logger.Warn("github sync event sink failed", "error", err)
	}
}

func (r *Runner) recordError(_ context.Context, binding db.IssueSyncBinding, startedAt time.Time, cause error, importResult db.ImportBatchResult) (RunResult, error) {
	cleanupCtx, cleanupCancel := r.cleanupContext()
	defer cleanupCancel()
	status, recordErr := r.config.Store.RecordIssueSyncError(cleanupCtx, db.IssueSyncErrorParams{
		BindingID: binding.ID,
		StartedAt: startedAt,
		At:        r.now(),
		Error:     cause.Error(),
	})
	if recordErr != nil {
		return RunResult{Binding: binding, Import: importResult}, recordErr
	}
	return RunResult{Binding: binding, Status: status, Import: importResult}, cause
}

func (r *Runner) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), runnerCleanupTimeout)
}

func (r *Runner) now() time.Time {
	return r.config.Clock().UTC()
}

func (r *Runner) validate() error {
	if r.config.Store == nil {
		return fmt.Errorf("github sync runner requires store")
	}
	if r.config.Fetcher == nil {
		return fmt.Errorf("github sync runner requires fetcher")
	}
	return nil
}

func (r *Runner) staleLockTTL() time.Duration {
	if r.config.StaleLockTTL <= 0 {
		return DefaultStaleLockTTL
	}
	return r.config.StaleLockTTL
}

func (r *Runner) initialBatchSize() int {
	if r.config.InitialBatchSize <= 0 {
		return DefaultInitialBatchSize
	}
	return r.config.InitialBatchSize
}

func syncSince(lastCursorAt *time.Time) *time.Time {
	if lastCursorAt == nil {
		return nil
	}
	since := lastCursorAt.Add(-2 * time.Minute)
	return &since
}

func mergeImportResult(dst *db.ImportBatchResult, src db.ImportBatchResult) {
	dst.Created += src.Created
	dst.Updated += src.Updated
	dst.Unchanged += src.Unchanged
	dst.Comments += src.Comments
	dst.Links += src.Links
	dst.Items = append(dst.Items, src.Items...)
	dst.Errors = append(dst.Errors, src.Errors...)
}

func orderImportItemsForLinkTargets(items []db.ImportItem) []db.ImportItem {
	if len(items) < 2 {
		return items
	}
	byExternalID := make(map[string]int, len(items))
	for i, item := range items {
		byExternalID[item.ExternalID] = i
	}
	ordered := make([]db.ImportItem, 0, len(items))
	state := make([]uint8, len(items))
	var visit func(int)
	visit = func(i int) {
		switch state[i] {
		case 2:
			return
		case 1:
			return
		}
		state[i] = 1
		for _, link := range items[i].Links {
			if targetIndex, ok := byExternalID[link.TargetExternalID]; ok {
				visit(targetIndex)
			}
		}
		state[i] = 2
		ordered = append(ordered, items[i])
	}
	for i := range items {
		visit(i)
	}
	return ordered
}

func errorsIsAlreadyRunning(err error) bool {
	return errors.Is(err, db.ErrIssueSyncAlreadyRunning)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
