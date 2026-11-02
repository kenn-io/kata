package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	v.Required = verificationRequired(issue)
	return records, v, nil
}

// CheckOpenRefutations applies the accepting daemon's close policy. Read-side
// verdicts are a point-in-time check, like the existing prose/throttle guards.
func CheckOpenRefutations(issue db.Issue, reason, policy string, overrule []string, records []commentref.Record, v db.Verification) ([]db.VerificationWarning, []string, error) {
	if reason != "done" && reason != "audit-no-change" {
		if len(overrule) > 0 {
			return nil, nil, api.NewError(400, "validation", "overrule requires done or audit-no-change", "", nil)
		}
		return nil, nil, nil
	}
	if policy == "" {
		policy = "warn"
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

func verificationRequired(issue db.Issue) bool {
	var values map[string]jsontext.Value
	if json.Unmarshal([]byte(issue.Metadata), &values) != nil {
		return false
	}
	var required bool
	return json.Unmarshal(values["verify.required"], &required) == nil && required
}

func checkCloseVerification(ctx context.Context, cfg ServerConfig, issue db.Issue, body api.CloseActionRequestBody, evidence []db.Evidence, issueUIDs *[]string) ([]db.VerificationWarning, []string, error) {
	// Verdict evidence pairing also applies to the owner-local TUI exception.
	for _, e := range body.Evidence {
		if e.Type == api.EvidenceConfirmed && body.Reason != "done" {
			return nil, nil, api.NewError(400, "validation", "confirmed evidence is only valid with done", "", nil)
		}
		if e.Type == api.EvidenceRefuted && body.Reason != "wontfix" && body.Reason != "duplicate" && body.Reason != "superseded" {
			return nil, nil, api.NewError(400, "validation", "--refuted is not valid with this reason; use --overrule for --done", "", nil)
		}
	}
	hasCitation := slices.ContainsFunc(body.Evidence, func(e api.Evidence) bool { return e.Type == api.EvidenceConfirmed || e.Type == api.EvidenceRefuted })
	if body.Reason != "done" && body.Reason != "audit-no-change" && !hasCitation {
		return CheckOpenRefutations(issue, body.Reason, cfg.CloseVerification, body.Overrule, nil, db.Verification{})
	}
	records, v, err := readVerification(ctx, cfg.DB, issue)
	if err != nil {
		return nil, nil, err
	}

	cited := false
	for index, e := range body.Evidence {
		if e.Type != api.EvidenceConfirmed && e.Type != api.EvidenceRefuted {
			continue
		}
		cited = cited || e.Type == api.EvidenceConfirmed
		claim, err := commentref.Resolve(records, issue.UID, e.Comment, projectNameForRecords(records, issue.UID))
		if err != nil || claim.IssueUID != issue.UID {
			return nil, nil, api.NewError(400, "validation", fmt.Sprintf("%s comment %q is not a live comment on this issue", e.Type, e.Comment), "", nil)
		}
		if claim.ReplyKind == "confirm" || claim.ReplyKind == "refute" {
			return nil, nil, api.NewError(400, "validation", "verdict evidence must cite the claim, not its confirm/refute reply", "", nil)
		}
		handle := commentref.HandleForIssue(claim.Handle, claim.IssueUID, claim.IssueShortID, issue.UID)
		evidence[index].Comment = claim.UID
		if !slices.Contains(*issueUIDs, claim.IssueUID) {
			*issueUIDs = append(*issueUIDs, claim.IssueUID)
		}
		verdict := v.Claims[handle]
		detail := ""
		switch {
		case !verdict.Eligible:
			detail = fmt.Sprintf("%s is superseded or is a verdict reply; cite a live claim", handle)
		case len(verdict.Confirmations) > 0 && len(verdict.OpenRefutations) > 0:
			detail = fmt.Sprintf("%s is contested: confirmed by %s, refuted by %s (open refutation)", handle, verdict.Confirmations[0], verdict.OpenRefutations[0])
		case e.Type == api.EvidenceConfirmed && len(verdict.OpenRefutations) > 0:
			detail = fmt.Sprintf("%s has an open refutation %s", handle, verdict.OpenRefutations[0])
		case e.Type == api.EvidenceConfirmed && len(verdict.Confirmations) == 0:
			detail = fmt.Sprintf("%s has no confirmation from another principal", handle)
		case e.Type == api.EvidenceRefuted && len(verdict.Confirmations) > 0:
			detail = fmt.Sprintf("%s has a counting confirmation; refuted evidence requires none", handle)
		case e.Type == api.EvidenceRefuted && len(verdict.IndependentRefutations) == 0:
			detail = fmt.Sprintf("%s has no open refutation from another principal", handle)
		}
		if detail != "" {
			return nil, nil, api.NewError(400, "validation", detail, "", nil)
		}
	}

	if body.Reason == "done" && v.Required && !cited {
		return nil, nil, api.NewError(409, "confirmation_required", "verify.required=true requires confirmed evidence citing a claim verified by another principal", fmt.Sprintf("kata notify %s --to <actor>/<teammate> --re c:<claim> --message \"Please confirm or refute\"", issue.ShortID), nil)
	}
	warnings, overruled, err := CheckOpenRefutations(issue, body.Reason, cfg.CloseVerification, body.Overrule, records, v)
	if err != nil {
		return nil, nil, err
	}
	canonical := func(handle string) string {
		r, err := commentref.Resolve(records, issue.UID, handle, projectNameForRecords(records, issue.UID))
		if err != nil {
			return ""
		} // Inputs were resolved in the same captured graph.
		if !slices.Contains(*issueUIDs, r.IssueUID) {
			*issueUIDs = append(*issueUIDs, r.IssueUID)
		}
		return r.UID
	}
	for i := range overruled {
		overruled[i] = canonical(overruled[i])
	}
	for i := range warnings {
		for j := range warnings[i].Refutations {
			r := &warnings[i].Refutations[j]
			r.Refutation = canonical(r.Refutation)
			r.Target = canonical(r.Target)
		}
	}
	return warnings, overruled, nil
}
