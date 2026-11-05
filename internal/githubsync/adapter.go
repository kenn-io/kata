package githubsync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type adapter struct{ config RunnerConfig }

func (*adapter) Provider() string     { return "github" }
func (*adapter) InitialPhase() string { return "repository" }

func (r *adapter) Prepare(ctx context.Context, binding db.IssueSyncBinding, syncStartedAt time.Time) (issuesync.Prepared, error) {
	if binding.Provider != "github" {
		err := fmt.Errorf("github sync runner cannot run provider %q", binding.Provider)
		return issuesync.Prepared{Binding: binding}, err
	}
	ghConfig, err := DecodeConfig(binding.Config)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	reconcileLegacyTitles := ghConfig.TitlePrefix == nil
	activeBinding := ghConfig.Binding()
	fetcher, err := r.fetcherForBinding(ctx, activeBinding)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	repo, err := fetcher.Repository(ctx, ghConfig.Host, ghConfig.Owner, ghConfig.Repo)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	if strings.TrimSpace(repo.NodeID) != binding.RemoteID {
		err := fmt.Errorf("github sync repository node mismatch: binding has %q, fetch returned %q", binding.RemoteID, repo.NodeID)
		return issuesync.Prepared{Binding: binding}, err
	}
	binding, ghConfig, err = r.refreshRepository(ctx, binding, ghConfig, repo, syncStartedAt)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	if refreshedBinding := ghConfig.Binding(); refreshedBinding != activeBinding {
		fetcher, err = r.fetcherForBinding(ctx, refreshedBinding)
		if err != nil {
			return issuesync.Prepared{Binding: binding}, err
		}
	}

	cutoff, err := ghConfig.SinceTime()
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	// Probe unsupported hosts before forcing a backfill REST pass. Otherwise
	// their permanently pending marker would force full issue reads every run.
	filteredBootstrap := binding.LastCursorAt == nil && cutoff != nil
	preflightParents := !filteredBootstrap && (binding.LastCursorAt == nil || ghConfig.NeedsParentLinkBackfill())
	var parentData ParentData
	if preflightParents {
		reportProgress(ctx, "parents", 0, 0)
		parentData, err = fetcher.ParentData(ctx, ghConfig.Binding(), ParentRequest{})
		if err != nil {
			return issuesync.Prepared{Binding: binding}, err
		}
	}
	parentLinkBackfill := ghConfig.NeedsParentLinkBackfill() && parentData.Scan == ParentScanComplete

	since := syncSince(binding.LastCursorAt)
	if reconcileLegacyTitles || parentLinkBackfill {
		since = nil
	}
	if cutoff != nil && (since == nil || cutoff.After(*since)) {
		since = cutoff
	}
	reportProgress(ctx, "issues", 0, 0)
	issues, err := fetcher.Issues(ctx, ghConfig.Binding(), since)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	if cutoff != nil {
		eligible := make([]Issue, 0, len(issues))
		for _, issue := range issues {
			if issue.UpdatedAt != nil && issue.UpdatedAt.After(*cutoff) {
				eligible = append(eligible, issue)
			}
		}
		issues = eligible
	}
	if !preflightParents {
		request, err := r.parentRequest(ctx, binding, issues, cutoff)
		if err != nil {
			return issuesync.Prepared{Binding: binding}, err
		}
		reportProgress(ctx, "parents", 0, len(request.IssueNumbers))
		parentData, err = fetcher.ParentData(ctx, ghConfig.Binding(), request)
		if err != nil {
			return issuesync.Prepared{Binding: binding}, err
		}
	}
	parentLinkBackfill = ghConfig.NeedsParentLinkBackfill() && parentData.Scan == ParentScanComplete
	comments, err := r.fetchComments(ctx, fetcher, binding, ghConfig, issues, cutoff)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}

	batch := BuildImportBatchWithConfig(binding.SourceKey, ghConfig, issues, comments, parentData, syncStartedAt)
	batch.ProjectID = binding.ProjectID
	batch.PreserveLocalParentConflicts = true
	if parentData.Scan == ParentScanComplete || parentData.Scan == ParentScanIncremental {
		batch.ReconcileLinkTypesForUnchanged = map[string]bool{"parent": true}
		batch.Items, err = r.appendScannedParentReconcileItems(ctx, batch, parentData, cutoff != nil)
		if err != nil {
			return issuesync.Prepared{Binding: binding}, err
		}
	}
	if parentLinkBackfill {
		batch.ReconcileLinkTypesForUnchanged = map[string]bool{"parent": true}
	}
	batch.Items, err = r.filterUnresolvableParentLinks(ctx, batch, cutoff != nil)
	if err != nil {
		return issuesync.Prepared{Binding: binding}, err
	}
	batch.Items = orderImportItemsForLinkTargets(batch.Items)
	prepared := issuesync.Prepared{Binding: binding, Batch: batch}
	for _, issue := range issues {
		if !IsPullRequestIssue(issue) && issue.ID > 0 && issue.Number > 0 {
			prepared.Locators = append(prepared.Locators, db.IssueStatusLocator{ExternalID: issueExternalID(issue), LegacyExternalIDs: statusLocatorAliases(issue), Locator: strconv.Itoa(issue.Number)})
		}
	}

	if parentLinkBackfill {
		prepared.Finalize = func(ctx context.Context) (db.IssueSyncBinding, error) {
			backfilledConfig := ghConfig.WithParentLinksBackfilled()
			configJSON, err := EncodeConfig(backfilledConfig)
			if err != nil {
				return binding, err
			}
			return r.config.Store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{
				BindingID:        binding.ID,
				DisplayName:      backfilledConfig.DisplayName(),
				Config:           configJSON,
				StartedAt:        &syncStartedAt,
				BindingUpdatedAt: new(binding.UpdatedAt),
			})
		}
	}
	return prepared, nil
}

// parentRequest selects the parent coverage for a run that skipped the
// repository-wide preflight scan. Without a cursor that is a filtered
// bootstrap of the eligible issues; otherwise it is an incremental pass over
// changed issues and recent relationship events.
func (r *adapter) parentRequest(ctx context.Context, binding db.IssueSyncBinding, issues []Issue, cutoff *time.Time) (ParentRequest, error) {
	numbers := make([]int, 0, len(issues))
	for _, issue := range issues {
		if !IsPullRequestIssue(issue) && issue.Number > 0 {
			numbers = append(numbers, issue.Number)
		}
	}
	if binding.LastCursorAt == nil {
		covered, err := r.selectedIssuesCoverImports(ctx, binding, issues)
		if err != nil || !covered {
			// Config changes can clear the cursor while keeping old imports.
			// Their parent-only changes still need authoritative coverage.
			return ParentRequest{}, err
		}
		return ParentRequest{IssueNumbers: numbers}, nil
	}
	// Relationship changes do not update updatedAt. Event discovery uses the
	// cursor overlap even if the issue cutoff excludes an already imported child.
	childrenOf, err := r.firstImportsBefore(ctx, binding, issues, cutoff)
	if err != nil {
		return ParentRequest{}, err
	}
	return ParentRequest{Since: syncSince(binding.LastCursorAt), IssueNumbers: numbers, ChildrenOf: childrenOf}, nil
}

// selectedIssuesCoverImports permits a scoped bootstrap only when every
// previously imported child is selected. A nil cursor can mean a config reset
// or a partial import retry, so it alone does not prove the binding is fresh.
func (r *adapter) selectedIssuesCoverImports(ctx context.Context, binding db.IssueSyncBinding, issues []Issue) (bool, error) {
	mappings, err := r.config.Store.ImportMappingsByProjectSource(ctx, binding.ProjectID, binding.SourceKey)
	if err != nil {
		return false, fmt.Errorf("lookup github bootstrap imports: %w", err)
	}
	selected := make(map[string]bool, len(issues))
	for _, issue := range issues {
		if IsPullRequestIssue(issue) || issue.Number <= 0 {
			continue
		}
		selected[issueExternalID(issue)] = true
		for _, alias := range issueLegacyExternalIDs(issue) {
			selected[alias] = true
		}
	}
	for _, mapping := range mappings {
		if mapping.ObjectType == "issue" && !selected[mapping.ExternalID] {
			return false, nil
		}
	}
	return true, nil
}

func (r *adapter) refreshRepository(ctx context.Context, binding db.IssueSyncBinding, ghConfig Config, repo Repository, startedAt time.Time) (db.IssueSyncBinding, Config, error) {
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return binding, ghConfig, fmt.Errorf("github repository full_name %q is invalid", repo.FullName)
	}
	refreshedConfig := Config{
		Since:              ghConfig.Since,
		StatusSync:         ghConfig.StatusSync,
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
	matches, err := db.IssueSyncConfigMatches(binding.Config, configJSON)
	if err != nil {
		return binding, ghConfig, err
	}
	if binding.DisplayName == refreshedConfig.DisplayName() && matches {
		return binding, refreshedConfig, nil
	}
	refreshed, err := r.config.Store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{
		BindingID:        binding.ID,
		DisplayName:      refreshedConfig.DisplayName(),
		Config:           configJSON,
		StartedAt:        &startedAt,
		BindingUpdatedAt: new(binding.UpdatedAt),
	})
	if err != nil {
		return binding, ghConfig, err
	}
	return refreshed, refreshedConfig, nil
}

func (r *adapter) fetcherForBinding(ctx context.Context, binding Binding) (Fetcher, error) {
	if sessionFetcher, ok := r.config.Fetcher.(BindingSessionFetcher); ok {
		return sessionFetcher.ForBinding(ctx, binding)
	}
	return r.config.Fetcher, nil
}

func (r *adapter) fetchComments(ctx context.Context, fetcher Fetcher, binding db.IssueSyncBinding, ghConfig Config, issues []Issue, cutoff *time.Time) (map[int][]Comment, error) {
	window := syncSince(binding.LastCursorAt)
	if window == nil {
		window = cutoff
	}
	var commented, older []Issue
	totalComments := 0
	for _, issue := range issues {
		totalComments += issue.Comments
		if IsPullRequestIssue(issue) || issue.Comments <= 0 {
			continue
		}
		commented = append(commented, issue)
		if window != nil && (issue.CreatedAt == nil || issue.CreatedAt.Before(*window)) {
			older = append(older, issue)
		}
	}
	fullHistory := make(map[int]bool)
	if binding.LastCursorAt == nil {
		// A cleared cursor can retain imports while missing comments made
		// before the new cutoff. Those issues need their full history again.
		for _, issue := range older {
			fullHistory[issue.Number] = true
		}
	} else {
		numbers, err := r.firstImportsBefore(ctx, binding, older, window)
		if err != nil {
			return nil, err
		}
		for _, number := range numbers {
			fullHistory[number] = true
		}
	}
	out := make(map[int][]Comment)
	var windowed, individual []Issue
	individualPages := 0
	for _, issue := range commented {
		if fullHistory[issue.Number] {
			individual = append(individual, issue)
		} else {
			windowed = append(windowed, issue)
			individualPages += (issue.Comments + 99) / 100
		}
	}
	// Without a cursor, PR comments can make the repository feed more
	// expensive than individual reads, or push it to the truncation limit.
	repositoryPages := (totalComments + 99) / 100
	if binding.LastCursorAt == nil && (repositoryPages >= repositoryCommentsPageLimit || repositoryPages > individualPages) {
		individual = append(individual, windowed...)
		windowed = nil
	}
	if len(windowed) > 0 {
		reportProgress(ctx, "comments", 0, 0)
		comments, complete, err := fetcher.RepositoryComments(ctx, ghConfig.Binding(), window)
		if err != nil {
			return nil, err
		}
		if complete {
			for _, issue := range windowed {
				out[issue.Number] = comments[issue.Number]
			}
		} else {
			individual = append(individual, windowed...)
		}
	}
	if len(individual) > 0 || len(commented) == 0 {
		reportProgress(ctx, "comments", 0, len(individual))
	}
	for i, issue := range individual {
		comments, err := fetcher.Comments(ctx, ghConfig.Binding(), issue.Number)
		if err != nil {
			return nil, err
		}
		out[issue.Number] = comments
		reportProgress(ctx, "comments", i+1, len(individual))
	}
	return out, nil
}

// firstImportsBefore returns issues created at or before the given time that
// have no import mapping yet.
func (r *adapter) firstImportsBefore(ctx context.Context, binding db.IssueSyncBinding, issues []Issue, before *time.Time) ([]int, error) {
	if before == nil {
		return nil, nil
	}
	var numbers []int
	for _, issue := range issues {
		if IsPullRequestIssue(issue) || issue.Number <= 0 || issue.CreatedAt == nil || issue.CreatedAt.After(*before) {
			continue
		}
		externalID := issueExternalID(issue)
		_, err := r.config.Store.ImportMappingBySource(ctx, binding.ProjectID, binding.SourceKey, "issue", externalID)
		if errors.Is(err, db.ErrNotFound) {
			numbers = append(numbers, issue.Number)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lookup github issue mapping %q: %w", externalID, err)
		}
	}
	return numbers, nil
}

func (r *adapter) appendScannedParentReconcileItems(ctx context.Context, batch db.ImportBatchParams, parentData ParentData, hasCutoff bool) ([]db.ImportItem, error) {
	if (parentData.Scan != ParentScanComplete && parentData.Scan != ParentScanIncremental) || len(parentData.ScannedChildIDs) == 0 {
		return batch.Items, nil
	}
	present := make(map[string]struct{}, len(batch.Items))
	for _, item := range batch.Items {
		present[item.ExternalID] = struct{}{}
	}
	numbers := make([]int, 0, len(parentData.ScannedChildIDs))
	for number := range parentData.ScannedChildIDs {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	items := append([]db.ImportItem(nil), batch.Items...)
	for _, number := range numbers {
		childExternalID := fmt.Sprintf("issue-id:%d", parentData.ScannedChildIDs[number])
		if _, ok := present[childExternalID]; ok {
			continue
		}
		mapping, err := r.config.Store.ImportMappingBySource(ctx, batch.ProjectID, batch.Source, "issue", childExternalID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				if !hasCutoff {
					r.config.Logger.Warn("github sync skipped parent reconciliation for unmapped scanned child",
						"source", batch.Source,
						"child_external_id", childExternalID,
						"child_number", number,
					)
				}

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

func (r *adapter) filterUnresolvableParentLinks(ctx context.Context, batch db.ImportBatchParams, hasCutoff bool) ([]db.ImportItem, error) {
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
					if !hasCutoff {
						r.config.Logger.Warn("github sync skipped unresolved parent link",
							"source", batch.Source,
							"child_external_id", items[i].ExternalID,
							"target_external_id", link.TargetExternalID,
						)
					}

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

func syncSince(lastCursorAt *time.Time) *time.Time {
	if lastCursorAt == nil {
		return nil
	}
	since := lastCursorAt.Add(-2 * time.Minute)
	return &since
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
