package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/teammate"
)

const moveBatchBudget = 4 * time.Second
const moveBatchChunkIssues = 50
const moveBatchClaimConcurrency = 16
const moveBatchPreviewTimeout = 45 * time.Second

type moveBatchItem struct {
	ctx    context.Context
	move   db.MoveIssueProjectIn
	issue  db.Issue
	target db.Project
	tree   []db.Issue
}

func moveBatchRefusal(result *api.MoveIssuesResult, err error) {
	if errors.Is(err, db.ErrTransactionFinalizationFailed) {
		err = internalAPIError(err)
	}
	mapped := moveIssueAPIError(err).(*api.APIError)
	result.Result = "refused"
	result.Code = mapped.Code
	result.Message = mapped.Message
	result.Hint = mapped.Hint
}

func moveBatchShouldPreview(result *api.MoveIssuesResult, item moveBatchItem, subtree bool) bool {
	if result.Result != "" {
		return false
	}
	if item.issue.ProjectID != item.target.ID {
		return true
	}
	if subtree {
		moveBatchRefusal(result, api.NewError(400, "same_project", "to_project_uid resolves to the issue's current project", "", nil))
	}
	return false
}

func previewMoveBatch(ctx context.Context, store db.Storage, items []moveBatchItem, results []api.MoveIssuesResult, indexes []int) error {
	for start := 0; start < len(indexes); start += moveBatchChunkIssues {
		part := indexes[start:min(start+moveBatchChunkIssues, len(indexes))]
		moves := make([]db.MoveIssueProjectIn, 0, len(part))
		for _, j := range part {
			moves = append(moves, items[j].move)
		}
		preview, err := store.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: moves, DryRun: true})
		if err != nil {
			return err
		}
		for k, j := range part {
			if preview.Results[k].Err != nil {
				moveBatchRefusal(&results[j], preview.Results[k].Err)
				continue
			}
			items[j].tree = preview.Results[k].Out.Issues
			db.FenceIssueMove(&items[j].move, preview.Results[k].Out)
		}
	}
	return nil
}

func checkMoveBatchClaims(ctx context.Context, cfg ServerConfig, items []moveBatchItem, results []api.MoveIssuesResult, indexes []int, actor string, cache *claimClientCache, refresh bool) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, moveBatchClaimConcurrency)
	for _, j := range indexes {
		if results[j].Result != "" || len(items[j].tree) == 0 {
			continue
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			itemCtx := items[j].ctx
			if itemCtx == nil {
				itemCtx = ctx
			}
			refusals := requireFederatedIssueClaimsConcurrently(itemCtx, cfg, items[j].tree, actor, cache, moveBatchClaimConcurrency, refresh)
			for _, issue := range items[j].tree {
				if err := refusals[issue.UID]; err != nil {
					moveBatchRefusal(&results[j], err)
					break
				}
			}
		})
	}
	wg.Wait()
}

func finishUnchangedMove(ctx context.Context, store db.Storage, item moveBatchItem, result *api.MoveIssuesResult, prior api.MoveIssuesResult) error {
	issue, _, err := resolveMoveBatchIssue(ctx, store, 0, item.issue.UID)
	if err != nil {
		if moveIssueAPIError(err).(*api.APIError).Status >= 500 {
			return err
		}
		moveBatchRefusal(result, err)
		return nil
	}
	want := item.issue.Revision
	if prior.Result != "" {
		want = prior.Revision
	}
	if issue.Revision != want {
		moveBatchRefusal(result, &db.RevisionConflictError{CurrentRevision: issue.Revision})
		return nil
	}
	if issue.ProjectID != item.target.ID {
		moveBatchRefusal(result, db.ErrIssueMoveTreeChanged)
		return nil
	}
	result.Result, result.Code, result.NewShortID, result.Revision = "unchanged", "already_in_target", issue.ShortID, issue.Revision
	if prior.Result != "" {
		result.Code = "duplicate_item"
	}
	result.To = item.target.Name + "#" + issue.ShortID
	return nil
}

func moveIssuesHandler(cfg ServerConfig) func(context.Context, *api.MoveIssuesRequest) (*api.MoveIssuesResponse, error) {
	return func(ctx context.Context, in *api.MoveIssuesRequest) (out *api.MoveIssuesResponse, resultErr error) {
		started := time.Now()
		if in.Body.DryRun {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, moveBatchPreviewTimeout)
			defer cancel()
			defer func() {
				apiErr, _ := errors.AsType[*api.APIError](resultErr)
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && (apiErr == nil || apiErr.Code != "access_unavailable") {
					out = nil
					resultErr = api.NewError(http.StatusRequestTimeout, "move_preview_timeout", "move preview timed out", "Reduce the number of items and retry.", nil)
				}
			}()
			if len(in.Body.Items) > 1 || in.Body.StartIndex != 0 || len(in.Body.Completed) != 0 {
				return nil, api.NewError(400, "validation", "dry run accepts one item and no continuation", "", nil)
			}
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if in.Body.StartIndex > len(in.Body.Items) {
			return nil, api.NewError(400, "validation", "start_index exceeds item count", "", nil)
		}
		if len(in.Body.Completed) != in.Body.StartIndex {
			return nil, api.NewError(400, "validation", "completed must contain one outcome for each prior item", "", nil)
		}
		if in.Body.Subtree && len(in.Body.Items) > 1 {
			return nil, api.NewError(400, "validation", "subtree move accepts one item", "", nil)
		}
		if err := validateMoveBatchOptions(in.Body.Comment, in.Body.Teammate); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		n := len(in.Body.Items)
		items := make([]moveBatchItem, n)
		for j := range items {
			//nolint:fatcontext // Each item derives from the same request, not the previous item.
			items[j].ctx = batchItemHostContext(ctx)
		}
		ctx = context.WithValue(ctx, deferBatchHostFenceKey{}, true)
		results := make([]api.MoveIssuesResult, n)
		targets := map[string]db.Project{}
		for j := in.Body.StartIndex; j < n; j++ {
			input := in.Body.Items[j]
			itemCtx := items[j].ctx
			authorize := func(uids []string) error { _, err := authorizeHostProjectScope(itemCtx, nil, uids, false); return err }
			r := &results[j]
			r.Index = j
			r.Ref = input.Ref
			if input.Ref == "" || input.ExpectedRevision < 0 {
				moveBatchRefusal(r, api.NewError(400, "validation", "ref must be nonempty and expected_revision must be nonnegative", "", nil))
				continue
			}
			uid := input.ToProjectUID
			if uid == "" {
				uid = in.Body.ToProjectUID
			}
			if uid == "" {
				moveBatchRefusal(r, api.NewError(400, "project_not_found", "target project is required", "", nil))
				continue
			}
			if err := authorize([]string{uid}); err != nil {
				moveBatchRefusal(r, err)
				continue
			}
			target, known := targets[uid]
			if !known {
				var err error
				target, err = activeProjectByUID(ctx, cfg.DB, uid)
				if err != nil {
					moveBatchRefusal(r, err)
					continue
				}
				targets[uid] = target
			}
			issue, source, err := resolveMoveBatchIssue(itemCtx, cfg.DB, in.Body.DefaultProjectID, input.Ref)
			if err != nil {
				moveBatchRefusal(r, err)
				continue
			}
			r.UID = issue.UID
			r.OldShortID = issue.ShortID
			r.FromProjectUID = source.UID
			r.ToProjectUID = target.UID
			r.ToProjectName = target.Name
			r.From = source.Name + "#" + issue.ShortID
			rev := input.ExpectedRevision
			if rev == 0 {
				rev = issue.Revision
			}
			items[j].issue = issue
			items[j].target = target
			items[j].move = db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: issue.ProjectID, ToProjectID: target.ID, IfMatchRev: rev, Actor: actor, Single: !in.Body.Subtree, AuthorizeProjects: authorize, BindContext: func(ctx context.Context) context.Context { return bindBatchItemHostContext(ctx, itemCtx) }}
		}
		cache := &claimClientCache{}
		succeeded, priorMoves, conflicts, err := seedMoveBatchContinuations(cfg.DB, in, items, results)
		if err != nil {
			return nil, moveBatchError(err, results[in.Body.StartIndex:], in.Body.StartIndex)
		}
		for j := in.Body.StartIndex; j < n; j++ {
			if results[j].Result != "" {
				continue
			}
			item, expected := items[j], in.Body.Items[j].ExpectedRevision
			prior := priorMoves[item.issue.ID]
			if expected != 0 && expected != item.issue.Revision && (!prior.moved || prior.targetUID != item.target.UID || prior.revision != item.issue.Revision || expected != prior.revision-1) {
				moveBatchRefusal(&results[j], &db.RevisionConflictError{CurrentRevision: item.issue.Revision})
			}
		}
		seen := map[int64][]int{}
		for j := in.Body.StartIndex; j < n; j++ {
			if results[j].Result == "" {
				seen[items[j].issue.ID] = append(seen[items[j].issue.ID], j)
			}
		}
		conflicted := func(id int64, indexes []int) bool {
			target := priorMoves[id].targetUID
			if conflicts[id] {
				return true
			}
			for _, j := range indexes {
				if results[j].Result != "" {
					continue
				}
				if target == "" {
					target = items[j].target.UID
				} else if target != items[j].target.UID {
					return true
				}
			}
			return false
		}
		var conflictPreviews []int
		for id, indexes := range seen {
			if conflicted(id, indexes) {
				for _, j := range indexes {
					if moveBatchShouldPreview(&results[j], items[j], in.Body.Subtree) {
						conflictPreviews = append(conflictPreviews, j)
					}
				}
			}
		}
		if err := previewMoveBatch(ctx, cfg.DB, items, results, conflictPreviews); err != nil {
			return nil, moveBatchError(err, results[in.Body.StartIndex:], in.Body.StartIndex)
		}
		checkMoveBatchClaims(ctx, cfg, items, results, conflictPreviews, actor, cache, !in.Body.DryRun)
		for id, indexes := range seen {
			if conflicted(id, indexes) {
				for _, j := range indexes {
					if results[j].Result == "" {
						results[j].Result, results[j].Code, results[j].Message = "refused", "conflicting_target", "issue requested for different targets"
					}
				}
			}
		}
		if in.Body.DryRun {
			return previewSingleMoveBatch(ctx, cfg, in, items, results, actor, cache)
		}
		out = &api.MoveIssuesResponse{}
		out.Body.NextIndex = in.Body.StartIndex
		budget := cfg.MoveBatchBudget
		if budget <= 0 {
			budget = moveBatchBudget
		}
		for start := in.Body.StartIndex; start < n; {
			end := min(start+moveBatchChunkIssues, n)
			pending := map[int64]bool{}
			var indexes, chunkPreviews, noops []int
			for j := start; j < end; j++ {
				if results[j].Result != "" {
					continue
				}
				item := &items[j]
				if _, ok := succeeded[item.issue.ID]; ok {
					noops = append(noops, j)
					continue
				}
				if pending[item.issue.ID] {
					end = j
					break
				}
				pending[item.issue.ID] = true
				if item.issue.ProjectID == item.target.ID {
					if in.Body.Subtree {
						moveBatchShouldPreview(&results[j], *item, true)
					} else {
						noops = append(noops, j)
					}
					continue
				}
				indexes = append(indexes, j)
				if len(item.tree) == 0 && in.Body.Subtree {
					chunkPreviews = append(chunkPreviews, j)
				} else if len(item.tree) == 0 {
					item.tree = []db.Issue{item.issue}
				}
			}
			if err := previewMoveBatch(ctx, cfg.DB, items, results, chunkPreviews); err != nil {
				return nil, moveBatchError(err, results[in.Body.StartIndex:], start)
			}
			checkMoveBatchClaims(ctx, cfg, items, results, indexes, actor, cache, !in.Body.DryRun)
			var write []db.MoveIssueProjectIn
			var written []int
			for _, j := range indexes {
				if results[j].Result != "" {
					continue
				}
				write = append(write, items[j].move)
				written = append(written, j)
			}
			if len(write) > 0 {
				committed, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: write, Comment: in.Body.Comment, Teammate: in.Body.Teammate})
				if err != nil {
					return nil, moveBatchError(err, results[in.Body.StartIndex:], start)
				}
				var events []db.Event
				for k, j := range written {
					item := committed.Results[k]
					if item.Err != nil {
						moveBatchRefusal(&results[j], item.Err)
						continue
					}
					r := &results[j]
					r.Result, r.NewShortID, r.ShortIDExtended = "moved", item.Out.NewShortID, len(item.Out.NewShortID) > shortid.MinLength
					r.EventID, r.Revision = item.Out.EventID, item.Out.NewRevision
					r.To, r.Descendants = items[j].target.Name+"#"+item.Out.NewShortID, item.Out.Issues[1:]
					succeeded[items[j].issue.ID] = *r
					events = append(events, item.Out.Events...)
				}
				cfg.Publish().EventsByProject(events)
			}
			for _, j := range noops {
				if err := finishUnchangedMove(items[j].ctx, cfg.DB, items[j], &results[j], succeeded[items[j].issue.ID]); err != nil {
					return nil, moveBatchError(err, results[in.Body.StartIndex:], start)
				}
				if results[j].Result == "unchanged" {
					succeeded[items[j].issue.ID] = results[j]
				}
			}
			out.Body.Results = append(out.Body.Results, results[start:end]...)
			out.Body.NextIndex = end
			start = end
			if time.Since(started) >= budget {
				break
			}
		}
		for _, r := range out.Body.Results {
			switch r.Result {
			case "moved":
				out.Body.Summary.Moved++
			case "unchanged":
				out.Body.Summary.Unchanged++
			case "refused":
				out.Body.Summary.Refused++
			case "would_move":
				out.Body.Summary.WouldMove++
			}
		}
		return out, nil
	}
}

func previewSingleMoveBatch(ctx context.Context, cfg ServerConfig, in *api.MoveIssuesRequest, items []moveBatchItem, results []api.MoveIssuesResult, actor string, cache *claimClientCache) (*api.MoveIssuesResponse, error) {
	var indexes, previews []int
	for j := range items {
		if results[j].Result != "" {
			continue
		}
		item := &items[j]
		if item.issue.ProjectID == item.target.ID {
			if in.Body.Subtree {
				moveBatchShouldPreview(&results[j], *item, true)
			} else if err := finishUnchangedMove(item.ctx, cfg.DB, *item, &results[j], api.MoveIssuesResult{}); err != nil {
				return nil, internalAPIError(err)
			}
			continue
		}
		indexes = append(indexes, j)
		if len(item.tree) == 0 {
			if in.Body.Subtree {
				previews = append(previews, j)
			} else {
				item.tree = []db.Issue{item.issue}
			}
		}
	}
	if err := previewMoveBatch(ctx, cfg.DB, items, results, previews); err != nil {
		return nil, internalAPIError(err)
	}
	checkMoveBatchClaims(ctx, cfg, items, results, indexes, actor, cache, false)
	var moves []db.MoveIssueProjectIn
	var selected []int
	for _, j := range indexes {
		if results[j].Result == "" {
			moves = append(moves, items[j].move)
			selected = append(selected, j)
		}
	}
	if len(moves) > 0 {
		rehearsal, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: moves, DryRun: true})
		if err != nil {
			return nil, internalAPIError(err)
		}
		for k, j := range selected {
			item := rehearsal.Results[k]
			if item.Err != nil {
				moveBatchRefusal(&results[j], item.Err)
				continue
			}
			results[j].Result = "would_move"
			results[j].Descendants = item.Out.Issues[1:]
		}
	}
	out := &api.MoveIssuesResponse{}
	out.Body.Results, out.Body.NextIndex = results, len(results)
	for _, result := range results {
		switch result.Result {
		case "would_move":
			out.Body.Summary.WouldMove++
		case "unchanged":
			out.Body.Summary.Unchanged++
		case "refused":
			out.Body.Summary.Refused++
		}
	}
	return out, nil
}

func moveBatchError(err error, results []api.MoveIssuesResult, next int) error {
	settled := make([]api.MoveIssuesResult, 0, len(results))
	for _, result := range results {
		if result.Result != "" {
			settled = append(settled, result)
		}
	}
	mapped := internalAPIError(err).(*api.APIError)
	mapped.Data = map[string]any{"results": settled, "next_index": next}
	return mapped
}

func resolveMoveBatchIssue(ctx context.Context, store db.Storage, projectID int64, ref string) (db.Issue, db.Project, error) {
	issue, err := resolveLinkTargetRef(ctx, store, projectID, ref, db.IncludeDeletedNo)
	var project db.Project
	if err == nil {
		project, err = activeProjectByID(ctx, store, issue.ProjectID)
	}
	if err == nil {
		_, err = authorizeHostProjectScope(ctx, []int64{issue.ProjectID}, nil, false)
	}
	if err == nil {
		err = authorizeIssueScopedIssue(ctx, store, issue)
	}
	return issue, project, err
}

func validateMoveBatchOptions(comment, handle string) error {
	if comment != "" && strings.TrimSpace(comment) == "" {
		return errors.New("comment must not be empty")
	}
	return teammate.Validate(handle)
}
