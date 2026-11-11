package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

func moveProof(key []byte, instance string, eventID int64, uid string, revision int64, actor string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%q:%q:%d:%q:%d:%q", "kata-move-continuation-v1", instance, eventID, uid, revision, actor)
	return hex.EncodeToString(mac.Sum(nil))
}

func validMoveProof(key []byte, instance string, completed api.MoveIssuesCompleted, actor string) bool {
	proof, err := hex.DecodeString(completed.Proof)
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(moveProof(key, instance, completed.EventID, completed.UID, completed.Revision, actor))
	return hmac.Equal(proof, expected)
}

type moveBatchPrior struct {
	targetID int64
	revision int64
}

func seedMoveBatchContinuations(ctx context.Context, cfg ServerConfig, in *api.MoveIssuesRequest, items []moveBatchItem, results []api.MoveIssuesResult, actor string, cache *claimClientCache) (map[int64]api.MoveIssuesResult, map[int64]moveBatchPrior, map[int64]bool, error) {
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
		itemCtx := items[j].ctx
		switch completed.Result {
		case "moved", "unchanged", "refused", "would_move":
		default:
			return nil, nil, nil, api.NewError(400, "validation", "invalid completed outcome", "", nil)
		}
		results[j].Result = completed.Result
		issue, relevant := pending[strings.ToUpper(completed.UID)]
		if !relevant || (completed.Result == "refused" && completed.Code != "conflicting_target") {
			continue
		}
		input := in.Body.Items[j]
		targetUID := input.ToProjectUID
		if targetUID == "" {
			targetUID = in.Body.ToProjectUID
		}
		target, err := activeProjectByUID(itemCtx, cfg.DB, targetUID)
		if err != nil {
			continue
		}
		if _, err := authorizeHostProjectScope(itemCtx, nil, []string{targetUID}, false); err != nil {
			continue
		}
		current, _, err := resolveMoveBatchIssue(itemCtx, cfg.DB, 0, issue.UID)
		if err != nil || current.ID != issue.ID {
			continue
		}
		if completed.Result == "moved" {
			valid, err := verifyMoveBatchEvent(itemCtx, cfg.DB, in.Body.DefaultProjectID, input, completed, current, target)
			if err != nil {
				return nil, nil, nil, err
			}
			if !valid {
				continue
			}
			items[j] = moveBatchItem{ctx: itemCtx, issue: current, target: target}
			results[j].Result = ""
			moves[current.ID] = moveBatchPrior{targetID: target.ID, revision: completed.Revision}
			succeeded[current.ID] = api.MoveIssuesResult{Result: "moved", Revision: completed.Revision}
			continue
		}
		resolved, _, err := resolveMoveBatchIssue(itemCtx, cfg.DB, in.Body.DefaultProjectID, input.Ref)
		if err != nil || resolved.ID != current.ID || (input.ExpectedRevision != 0 && input.ExpectedRevision != current.Revision) {
			continue
		}
		if completed.Result == "refused" && completed.Code == "conflicting_target" {
			conflicts[current.ID] = true
			continue
		}
		if completed.Result == "would_move" && in.Body.DryRun && current.ProjectID != target.ID {
			items[j] = moveBatchItem{ctx: itemCtx, issue: current, target: target, move: db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: current.ProjectID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: actor, Single: !in.Body.Subtree, AuthorizeProjects: func(uids []string) error { _, err := authorizeHostProjectScope(itemCtx, nil, uids, false); return err }, BindContext: func(ctx context.Context) context.Context { return bindBatchItemHostContext(ctx, itemCtx) }}}
			results[j].Result = ""
			if err := previewMoveBatch(ctx, cfg.DB, items, results, []int{j}); err != nil {
				return nil, nil, nil, err
			}
			if results[j].Result == "" {
				refusals := requireFederatedIssueClaimsConcurrently(itemCtx, cfg, items[j].tree, actor, cache, moveBatchClaimConcurrency, false)
				for _, member := range items[j].tree {
					if err := refusals[member.UID]; err != nil {
						moveBatchRefusal(&results[j], err)
						break
					}
				}
			}
			if results[j].Result == "" {
				succeeded[current.ID] = api.MoveIssuesResult{Result: "would_move"}
			}
			continue
		}
		if completed.Result == "unchanged" && current.ProjectID == target.ID {
			items[j] = moveBatchItem{ctx: itemCtx, issue: current, target: target}
			results[j].Result = ""
			succeeded[current.ID] = api.MoveIssuesResult{Result: "unchanged", Revision: current.Revision}
		}
	}
	return succeeded, moves, conflicts, nil
}

func verifyMoveBatchEvent(ctx context.Context, store db.Storage, defaultProjectID int64, input api.MoveIssuesItem, completed api.MoveIssuesCompleted, issue db.Issue, target db.Project) (bool, error) {
	if completed.EventID <= 0 || completed.Revision <= 1 || completed.Revision != issue.Revision || issue.ProjectID != target.ID || (input.ExpectedRevision != 0 && input.ExpectedRevision != completed.Revision-1) {
		return false, nil
	}
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: completed.EventID - 1, ThroughID: completed.EventID, IssueUID: issue.UID, Types: []string{"issue.moved"}, Limit: 1})
	if err != nil {
		return false, err
	}
	if len(events) != 1 || events[0].ID != completed.EventID || events[0].OriginInstanceUID != store.InstanceUID() {
		return false, nil
	}
	var payload struct {
		FromProjectUID string `json:"from_project_uid"`
		ToProjectUID   string `json:"to_project_uid"`
		Revision       int64  `json:"revision"`
	}
	if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil || payload.ToProjectUID != target.UID || payload.Revision != completed.Revision {
		return false, nil
	}
	ref, err := shortid.Parse(input.Ref)
	if err != nil {
		return false, nil
	}
	if ref.ULID != "" {
		return strings.EqualFold(ref.ULID, issue.UID), nil
	}
	var project db.Project
	if ref.Project != "" {
		project, err = store.ProjectByNameIncludingArchived(ctx, ref.Project)
	} else {
		project, err = store.ProjectByID(ctx, defaultProjectID)
	}
	if err != nil {
		return false, nil
	}
	if _, err := authorizeHostProjectScope(ctx, []int64{project.ID}, []string{payload.FromProjectUID}, false); err != nil {
		return false, nil
	}
	// Resolve live or retained aliases to the stable UID, rejecting reassignment.
	resolved, err := store.IssueByShortID(ctx, project.ID, ref.ShortID, db.IncludeDeletedNo)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return false, err
	}
	return err == nil && resolved.UID == issue.UID, nil
}
