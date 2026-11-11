package daemon

import (
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

type moveBatchPrior struct {
	targetUID string
	revision  int64
	moved     bool
}

func seedMoveBatchContinuations(store db.Storage, in *api.MoveIssuesRequest, items []moveBatchItem, results []api.MoveIssuesResult) (map[int64]api.MoveIssuesResult, map[int64]moveBatchPrior, map[int64]bool, error) {
	succeeded := map[int64]api.MoveIssuesResult{}
	moves := map[int64]moveBatchPrior{}
	conflicts := map[int64]bool{}
	pending := map[string]db.Issue{}
	for j := in.Body.StartIndex; j < len(items); j++ {
		if results[j].Result == "" {
			pending[strings.ToUpper(items[j].issue.UID)] = items[j].issue
		}
	}
	for j, completed := range in.Body.Completed {
		switch completed.Result {
		case "moved", "unchanged", "refused":
		default:
			return nil, nil, nil, api.NewError(400, "validation", "invalid completed outcome", "", nil)
		}
		results[j].Result = completed.Result
		issue, relevant := pending[strings.ToUpper(completed.UID)]
		if !relevant || (completed.Result == "refused" && completed.Code != "conflicting_target") {
			continue
		}
		itemCtx := items[j].ctx
		ref, err := shortid.Parse(in.Body.Items[j].Ref)
		if err != nil {
			continue
		}
		if ref.ULID == "" {
			var project db.Project
			if ref.Project != "" {
				project, err = store.ProjectByNameIncludingArchived(itemCtx, ref.Project)
			} else {
				project, err = store.ProjectByID(itemCtx, in.Body.DefaultProjectID)
			}
			if err == nil {
				_, err = authorizeHostProjectScope(itemCtx, []int64{project.ID}, nil, false)
			}
			if err == nil {
				err = authorizeIssueScopedProject(itemCtx, project)
			}
		}
		if err == nil {
			var resolved db.Issue
			resolved, _, err = resolveMoveBatchIssue(itemCtx, store, in.Body.DefaultProjectID, in.Body.Items[j].Ref)
			if err == nil && resolved.ID != issue.ID {
				continue
			}
		}
		if err != nil {
			if moveIssueAPIError(err).(*api.APIError).Status >= 500 {
				return nil, nil, nil, err
			}
			continue
		}
		id := issue.ID
		if completed.Result == "refused" {
			if completed.Code == "conflicting_target" {
				conflicts[id] = true
			}
			continue
		}
		target := in.Body.Items[j].ToProjectUID
		if target == "" {
			target = in.Body.ToProjectUID
		}
		moves[id] = moveBatchPrior{targetUID: target, revision: completed.Revision, moved: completed.Result == "moved" || moves[id].moved}
		if issue.Revision == completed.Revision {
			succeeded[id] = api.MoveIssuesResult{Result: completed.Result, Revision: completed.Revision}
		}
	}
	return succeeded, moves, conflicts, nil
}
