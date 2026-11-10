package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

const moveBatchBudget = 4 * time.Second
const moveBatchChunkIssues = 50
const moveBatchClaimConcurrency = 16

type moveBatchItem struct {
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

func moveIssuesHandler(cfg ServerConfig) func(context.Context, *api.MoveIssuesRequest) (*api.MoveIssuesResponse, error) {
	return func(ctx context.Context, in *api.MoveIssuesRequest) (*api.MoveIssuesResponse, error) {
		started := time.Now()
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if in.Body.StartIndex > len(in.Body.Items) {
			return nil, api.NewError(400, "validation", "start_index exceeds item count", "", nil)
		}
		if in.Body.Subtree && len(in.Body.Items) > 1 {
			return nil, api.NewError(400, "validation", "subtree move accepts one item", "", nil)
		}
		if err := teammate.Validate(in.Body.Teammate); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		n := len(in.Body.Items)
		items := make([]moveBatchItem, n)
		results := make([]api.MoveIssuesResult, n)
		targets := map[string]db.Project{}
		targetErrors := map[string]error{}
		residentPrefix := map[[2]int64]bool{}
		authorize := func(uids []string) error { _, err := authorizeHostProjectScope(ctx, nil, uids, false); return err }
		for j, input := range in.Body.Items {
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
			target, known := targets[uid]
			if !known && targetErrors[uid] == nil {
				if uid == "" {
					err = api.NewError(400, "project_not_found", "target project is required", "", nil)
				} else {
					target, err = activeProjectByUID(ctx, cfg.DB, uid)
				}
				if err == nil {
					err = authorize([]string{uid})
				}
				if err != nil {
					targetErrors[uid] = err
				} else {
					targets[uid] = target
				}
			}
			if err := targetErrors[uid]; err != nil {
				moveBatchRefusal(r, err)
				continue
			}
			issue, err := resolveLinkTargetRef(ctx, cfg.DB, in.Body.DefaultProjectID, input.Ref, db.IncludeDeletedNo)
			var source db.Project
			if err == nil {
				source, err = activeProjectByID(ctx, cfg.DB, issue.ProjectID)
			}
			if err == nil {
				_, err = authorizeHostProjectScope(ctx, []int64{issue.ProjectID}, nil, false)
			}
			if err == nil {
				err = authorizeIssueScopedIssue(ctx, cfg.DB, issue)
			}
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
			items[j].move = db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: issue.ProjectID, ToProjectID: target.ID, IfMatchRev: rev, Actor: actor, Single: !in.Body.Subtree, AuthorizeProjects: authorize}
			key := [2]int64{issue.ID, target.ID}
			if j < in.Body.StartIndex && issue.ProjectID == target.ID {
				residentPrefix[key] = true
			}
			// A prior row may have moved this issue and advanced its revision once.
			completedDuplicate := j >= in.Body.StartIndex && residentPrefix[key] && issue.ProjectID == target.ID && input.ExpectedRevision == issue.Revision-1
			if input.ExpectedRevision != 0 && input.ExpectedRevision != issue.Revision && !completedDuplicate {
				moveBatchRefusal(r, &db.RevisionConflictError{CurrentRevision: issue.Revision})
			}
		}
		// Validate every candidate before comparing targets, including the skipped prefix.
		for start := 0; start < n; start += moveBatchChunkIssues {
			end := min(start+moveBatchChunkIssues, n)
			var moves []db.MoveIssueProjectIn
			var indexes []int
			for j := start; j < end; j++ {
				if moveBatchShouldPreview(&results[j], items[j], in.Body.Subtree) {
					moves = append(moves, items[j].move)
					indexes = append(indexes, j)
				}
			}
			if len(moves) == 0 {
				continue
			}
			preview, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: moves, DryRun: true})
			if err != nil {
				return nil, moveBatchError(err, nil, in.Body.StartIndex)
			}
			for k, j := range indexes {
				if preview.Results[k].Err != nil {
					moveBatchRefusal(&results[j], preview.Results[k].Err)
					continue
				}
				items[j].tree = preview.Results[k].Out.Issues
				db.FenceIssueMove(&items[j].move, preview.Results[k].Out)
			}
		}
		seen := map[int64][]int{}
		for j := range items {
			if results[j].Result == "" {
				seen[items[j].issue.ID] = append(seen[items[j].issue.ID], j)
			}
		}
		for _, indexes := range seen {
			target := items[indexes[0]].target.UID
			conflict := false
			for _, j := range indexes {
				conflict = conflict || items[j].target.UID != target
			}
			if conflict {
				for _, j := range indexes {
					results[j].Result, results[j].Code, results[j].Message = "refused", "conflicting_target", "issue requested for different targets"
				}
			}
		}
		out := &api.MoveIssuesResponse{}
		cache := &claimClientCache{}
		budget := cfg.MoveBatchBudget
		if budget <= 0 {
			budget = moveBatchBudget
		}
		succeeded := map[int64]api.MoveIssuesResult{}
		for start := in.Body.StartIndex; start < n; {
			end := min(start+moveBatchChunkIssues, n)
			pending := map[int64]bool{}
			var indexes []int
			var claimed []db.Issue
			for j := start; j < end; j++ {
				if results[j].Result != "" {
					continue
				}
				item := &items[j]
				if prior, ok := succeeded[item.issue.ID]; ok {
					r := &results[j]
					r.Result, r.Code = "unchanged", "duplicate_item"
					if prior.Result == "would_move" {
						r.Result = "would_move"
					}
					r.NewShortID, r.To = prior.NewShortID, prior.To
					continue
				}
				if pending[item.issue.ID] {
					end = j
					break
				}
				pending[item.issue.ID] = true
				if item.issue.ProjectID == item.target.ID {
					r := &results[j]
					r.Result, r.Code, r.NewShortID = "unchanged", "already_in_target", item.issue.ShortID
					r.To = item.target.Name + "#" + item.issue.ShortID
					succeeded[item.issue.ID] = *r
					continue
				}
				indexes = append(indexes, j)
				claimed = append(claimed, item.tree...)
			}
			refusals := requireFederatedIssueClaimsConcurrently(ctx, cfg, claimed, actor, cache, moveBatchClaimConcurrency, !in.Body.DryRun)
			var write []db.MoveIssueProjectIn
			var written []int
			for _, j := range indexes {
				for _, issue := range items[j].tree {
					if err := refusals[issue.UID]; err != nil {
						moveBatchRefusal(&results[j], err)
						break
					}
				}
				if results[j].Result != "" {
					continue
				}
				if in.Body.DryRun {
					results[j].Result = "would_move"
					results[j].Descendants = items[j].tree[1:]
					succeeded[items[j].issue.ID] = results[j]
					continue
				}
				write = append(write, items[j].move)
				written = append(written, j)
			}
			if len(write) > 0 {
				committed, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: write, Comment: in.Body.Comment, Teammate: in.Body.Teammate})
				if err != nil {
					return nil, moveBatchError(err, out.Body.Results, start)
				}
				var events []db.Event
				for k, j := range written {
					item := committed.Results[k]
					if item.Err != nil {
						moveBatchRefusal(&results[j], item.Err)
						continue
					}
					r := &results[j]
					r.Result, r.NewShortID, r.ShortIDExtended = "moved", item.Out.NewShortID, len(item.Out.NewShortID) > len(items[j].issue.ShortID)
					r.EventID, r.Revision = item.Out.EventID, item.Out.NewRevision
					r.To, r.Descendants = items[j].target.Name+"#"+item.Out.NewShortID, item.Out.Issues[1:]
					succeeded[items[j].issue.ID] = *r
					events = append(events, item.Out.Events...)
				}
				cfg.Publish().EventsByProject(events)
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

func moveBatchError(err error, results []api.MoveIssuesResult, next int) error {
	mapped := internalAPIError(err).(*api.APIError)
	mapped.Data = map[string]any{"results": results, "next_index": next}
	return mapped
}
