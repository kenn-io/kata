package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
)

// readVerification uses the full authorized project graph. The selected graph
// only loads immediate neighbors and would omit cross-issue rebuttal chains.
func readVerification(ctx context.Context, store db.Storage, issue db.Issue) ([]commentref.Record, db.Verification, error) {
	data, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: issue.ProjectID, IssueScope: issueScopeFromContext(ctx), VerificationIssueID: issue.ID})
	if err != nil {
		return nil, db.Verification{}, internalAPIError(err)
	}
	records, err := projectCommentGraph(ctx, data)
	if err != nil {
		return nil, db.Verification{}, err
	}
	v := commentref.Evaluate(records, issue.UID, nil)
	v.Redacted += data.Redacted
	return records, v, nil
}

// CheckOpenRefutations applies the accepting daemon's close policy. Read-side
// verdicts are a point-in-time check, like the existing prose/throttle guards.
func CheckOpenRefutations(ctx context.Context, store db.Storage, issue db.Issue, reason, policy string, overrule []string) ([]db.VerificationWarning, []string, error) {
	if reason != "done" && reason != "audit-no-change" {
		if len(overrule) > 0 {
			return nil, nil, api.NewError(400, "validation", "overrule requires done or audit-no-change", "", nil)
		}
		return nil, nil, nil
	}
	if policy == "" {
		policy = "warn"
	}
	if policy == "off" && len(overrule) == 0 {
		return nil, nil, nil
	}
	records, v, err := readVerification(ctx, store, issue)
	if err != nil {
		return nil, nil, err
	}
	overruled := []string{}
	for _, input := range overrule {
		r, err := commentref.Resolve(records, issue.UID, input, projectNameForRecords(records, issue.UID))
		if err != nil {
			return nil, nil, api.NewError(400, "validation", fmt.Sprintf("overrule %q is not an open refutation", input), "", nil)
		}
		handle := commentref.HandleForIssue(r.Handle, r.IssueUID, r.IssueShortID, issue.UID)
		if !slices.ContainsFunc(v.OpenRefutations, func(r db.OpenRefutation) bool { return r.Refutation == handle }) {
			return nil, nil, api.NewError(400, "validation", fmt.Sprintf("overrule %q is not an open refutation", input), "", nil)
		}
		if !slices.Contains(overruled, handle) {
			overruled = append(overruled, handle)
		}
	}
	unresolved := []db.OpenRefutation{}
	for _, r := range v.OpenRefutations {
		if !slices.Contains(overruled, r.Refutation) {
			unresolved = append(unresolved, r)
		}
	}
	if policy == "block" && len(unresolved) > 0 {
		lines := []string{}
		for _, r := range unresolved[:min(len(unresolved), 10)] {
			author := r.Author
			if r.Teammate != "" {
				author += "/" + r.Teammate
			}
			lines = append(lines, fmt.Sprintf("%s  %s  %s", r.Refutation, author, r.Summary))
		}
		return nil, nil, api.NewError(409, "open_refutations", fmt.Sprintf("%d open refutations:\n%s", len(unresolved), strings.Join(lines, "\n")), "supersede the claim, withdraw or rebut the refutation, or close with --overrule <refutation>", nil)
	}
	if policy == "warn" && len(unresolved) > 0 {
		return []db.VerificationWarning{{Code: "open_refutations", Refutations: unresolved}}, overruled, nil
	}
	return nil, overruled, nil
}

func projectNameForRecords(records []commentref.Record, issueUID string) string {
	for _, r := range records {
		if r.IssueUID == issueUID {
			return r.ProjectName
		}
	}
	return ""
}
