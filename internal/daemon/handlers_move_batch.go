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
	owner  int
}

func moveBatchRefusal(result *api.MoveIssuesResult, err error) {
	if errors.Is(err, db.ErrTransactionFinalizationFailed) {
		err = internalAPIError(err)
	}
	mapped, ok := errors.AsType[*api.APIError](err)
	if !ok {
		mapped = moveIssueAPIError(err).(*api.APIError)
	}
	result.Result = "refused"
	result.Code = mapped.Code
	result.Message = mapped.Message
	result.Hint = mapped.Hint
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
		pairs := map[[2]int64]error{}
		targetErrors := map[string]error{}
		seen := map[int64][]int{}
		authorize := func(uids []string) error { _, err := authorizeHostProjectScope(ctx, nil, uids, false); return err }
		for j, input := range in.Body.Items {
			r := &results[j]
			r.Index = j
			r.Ref = input.Ref
			items[j].owner = -1
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
			if err == nil {
				_, err = activeProjectByID(ctx, cfg.DB, issue.ProjectID)
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
			source, err := cfg.DB.ProjectByID(ctx, issue.ProjectID)
			if err != nil {
				return nil, internalAPIError(err)
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
			seen[issue.ID] = append(seen[issue.ID], j)
			if input.ExpectedRevision != 0 && input.ExpectedRevision != issue.Revision {
				moveBatchRefusal(r, &db.RevisionConflictError{CurrentRevision: issue.Revision})
			}
			pair := [2]int64{issue.ProjectID, target.ID}
			if issue.ProjectID != target.ID {
				if _, known := pairs[pair]; !known {
					pairs[pair] = cfg.DB.CheckIssueMovePair(ctx, pair[0], pair[1])
				}
				if err := pairs[pair]; err != nil {
					moveBatchRefusal(r, err)
					continue
				}
			}
		}
		for _, indexes := range seen {
			target := items[indexes[0]].target.UID
			conflict := false
			for _, j := range indexes {
				conflict = conflict || items[j].target.UID != target
			}
			owner := -1
			for _, j := range indexes {
				if conflict {
					results[j].Result = "refused"
					results[j].Code = "conflicting_target"
					results[j].Message = "issue requested for different targets"
				} else if j >= in.Body.StartIndex && results[j].Result == "" {
					if owner < 0 {
						owner = j
					} else {
						items[j].owner = owner
					}
				}
			}
		}
		for j := range items {
			item := &items[j]
			if results[j].Result == "" && item.owner < 0 && item.issue.ProjectID == item.target.ID {
				results[j].Result = "unchanged"
				results[j].Code = "already_in_target"
				results[j].NewShortID = item.issue.ShortID
				results[j].To = item.target.Name + "#" + item.issue.ShortID
			}
		}
		out := &api.MoveIssuesResponse{}
		cache := &claimClientCache{}
		budget := cfg.MoveBatchBudget
		if budget <= 0 {
			budget = moveBatchBudget
		}
		for start := in.Body.StartIndex; start < n; {
			// A duplicate may proceed when its earlier attempt was refused.
			if owner := items[start].owner; owner >= 0 && results[owner].Result == "refused" {
				for j := start + 1; j < n; j++ {
					if items[j].owner == owner {
						items[j].owner = start
					}
				}
				items[start].owner = -1
			}
			end := min(start+moveBatchChunkIssues, n)
			// Finish the owner's chunk before deciding the duplicate's outcome.
			for j := start + 1; j < end; j++ {
				owner := items[j].owner
				if owner >= 0 && results[j].Result == "" && (owner >= start || results[owner].Result == "refused") {
					end = j
					break
				}
			}
			var moves []db.MoveIssueProjectIn
			var indexes []int
			for j := start; j < end; j++ {
				if results[j].Result == "" && items[j].owner < 0 {
					moves = append(moves, items[j].move)
					indexes = append(indexes, j)
				}
			}
			if len(moves) > 0 {
				preview, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: moves, DryRun: true, Actor: actor, AuthorizeProjects: authorize})
				if err != nil {
					return nil, moveBatchError(err, out.Body.Results, start)
				}
				var claimed []db.Issue
				for k, j := range indexes {
					if preview.Results[k].Err != nil {
						moveBatchRefusal(&results[j], preview.Results[k].Err)
						continue
					}
					items[j].tree = preview.Results[k].Out.Issues
					items[j].move.ExpectedIssueIDs = preview.Results[k].Out.TreeIssueIDs
					claimed = append(claimed, items[j].tree...)
				}
				refusals := requireFederatedIssueClaimsConcurrently(ctx, cfg, claimed, actor, cache, moveBatchClaimConcurrency, !in.Body.DryRun)
				var write []db.MoveIssueProjectIn
				var written []int
				for _, j := range indexes {
					if results[j].Result != "" {
						continue
					}
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
						continue
					}
					write = append(write, items[j].move)
					written = append(written, j)
				}
				if len(write) > 0 {
					committed, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: write, Actor: actor, Comment: in.Body.Comment, Teammate: in.Body.Teammate, AuthorizeProjects: authorize})
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
						r.Result = "moved"
						r.NewShortID = item.Out.NewShortID
						r.ShortIDExtended = item.ShortIDExtended
						r.EventID = item.Out.EventID
						r.Revision = item.Out.NewRevision
						r.To = items[j].target.Name + "#" + item.Out.NewShortID
						r.Descendants = item.Out.Issues[1:]
						events = append(events, item.Out.Events...)
					}
					cfg.Publish().EventsByProject(events)
				}
			}
			for j := start; j < end; j++ {
				if owner := items[j].owner; owner >= 0 && results[j].Result == "" {
					first := results[owner]
					r := &results[j]
					r.Result, r.Code, r.Message, r.Hint = first.Result, first.Code, first.Message, first.Hint
					switch first.Result {
					case "moved", "unchanged":
						r.Result, r.Code = "unchanged", "duplicate_item"
						r.NewShortID, r.To = first.NewShortID, first.To
					case "would_move":
						r.Code = "duplicate_item"
					}
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

func moveBatchError(err error, results []api.MoveIssuesResult, next int) error {
	mapped := internalAPIError(err).(*api.APIError)
	mapped.Data = map[string]any{"results": results, "next_index": next}
	return mapped
}
