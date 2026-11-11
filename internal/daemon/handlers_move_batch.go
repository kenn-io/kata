package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/teammate"
)

const moveBatchClaimConcurrency = 16

type moveBatchItem struct {
	ctx    context.Context
	move   db.MoveIssueProjectIn
	issue  db.Issue
	target db.Project
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

func checkMoveBatchClaims(cfg ServerConfig, items []moveBatchItem, results []api.MoveIssuesResult, indexes []int, actor string) {
	cache := &claimClientCache{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, moveBatchClaimConcurrency)
	for _, j := range indexes {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := requireFederatedIssueClaimWithRefresh(items[j].ctx, cfg, items[j].issue, actor, true, cache); err != nil {
				moveBatchRefusal(&results[j], err)
			}
		})
	}
	wg.Wait()
}

func moveIssuesHandler(cfg ServerConfig) func(context.Context, *api.MoveIssuesRequest) (*api.MoveIssuesResponse, error) {
	return func(ctx context.Context, in *api.MoveIssuesRequest) (out *api.MoveIssuesResponse, resultErr error) {
		if len(in.Body.Items) == 0 || len(in.Body.Items) > api.MoveBatchMaxItems {
			return nil, api.NewError(400, "validation", "items must contain between 1 and 50 entries", "", nil)
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
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
		for j := range n {
			input := in.Body.Items[j]
			itemCtx := items[j].ctx
			authorize := func(uids []string) error { _, err := authorizeHostProjectScope(itemCtx, nil, uids, false); return err }
			r := &results[j]
			r.Index = j
			r.Ref = input.Ref
			if input.Invalid || input.Ref == "" || input.ExpectedRevision < 0 {
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
			items[j].move = db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: issue.ProjectID, ToProjectID: target.ID, IfMatchRev: rev, Actor: actor, AuthorizeProjects: authorize, BindContext: func(ctx context.Context) context.Context { return bindBatchItemHostContext(ctx, itemCtx) }}
		}
		var indexes []int
		for j := range items {
			if results[j].Result != "" {
				continue
			}
			item, expected := items[j], in.Body.Items[j].ExpectedRevision
			if expected != 0 && expected != item.issue.Revision {
				moveBatchRefusal(&results[j], &db.RevisionConflictError{CurrentRevision: item.issue.Revision})
				continue
			}
			if item.issue.ProjectID != item.target.ID {
				indexes = append(indexes, j)
			}
		}
		checkMoveBatchClaims(cfg, items, results, indexes, actor)
		var moves []db.MoveIssueProjectIn
		var selected []int
		for j := range items {
			if results[j].Result == "" {
				moves = append(moves, items[j].move)
				selected = append(selected, j)
			}
		}
		if len(moves) > 0 {
			committed, err := cfg.DB.MoveIssuesBatch(ctx, db.MoveIssuesBatchIn{Items: moves, Comment: in.Body.Comment, Teammate: in.Body.Teammate})
			if err != nil {
				return nil, moveBatchError(err, results)
			}
			var events []db.Event
			for k, j := range selected {
				item := committed.Results[k]
				if errors.Is(item.Err, db.ErrDuplicateMoveTarget) {
					results[j].Result, results[j].Code, results[j].Message = "refused", "duplicate_conflicting_target", item.Err.Error()
					continue
				}
				if item.Err != nil {
					moveBatchRefusal(&results[j], item.Err)
					continue
				}
				r := &results[j]
				r.Result, r.Code, r.NewShortID, r.Revision = "unchanged", item.Outcome, item.Out.NewShortID, item.Out.NewRevision
				if item.Outcome == "moved" {
					r.Result, r.Code, r.ShortIDExtended, r.EventID = "moved", "", len(item.Out.NewShortID) > shortid.MinLength, item.Out.EventID
				}
				r.To = items[j].target.Name + "#" + item.Out.NewShortID
				events = append(events, item.Out.Events...)
			}
			cfg.Publish().EventsByProject(events)
		}
		out = &api.MoveIssuesResponse{}
		out.Body.Results = results
		for _, r := range out.Body.Results {
			switch r.Result {
			case "moved":
				out.Body.Summary.Moved++
			case "unchanged":
				out.Body.Summary.Unchanged++
			case "refused":
				out.Body.Summary.Refused++
			}
		}
		return out, nil
	}
}

func moveBatchError(err error, results []api.MoveIssuesResult) error {
	settled := make([]api.MoveIssuesResult, 0, len(results))
	for _, result := range results {
		if result.Result != "" {
			settled = append(settled, result)
		}
	}
	mapped := internalAPIError(err).(*api.APIError)
	mapped.Data = map[string]any{"results": settled}
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
