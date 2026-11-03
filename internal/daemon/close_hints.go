package daemon

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func shellHintArg(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func closeShowCommand(ctx context.Context, store db.Storage, issue db.Issue) (string, error) {
	project, err := store.ProjectByID(ctx, issue.ProjectID)
	if err != nil {
		return "", err
	}
	return "kata show " + shellHintArg(project.Name+"#"+issue.ShortID), nil
}

// A show command is safe even when the request has no claim or reviewer. The
// closer chooses the intended completion claim; we never select a reproduction
// or invent a reviewer, evidence text, or a reopening mutation for them.
func closeRefusalHint(ctx context.Context, store db.Storage, issue db.Issue, body api.CloseActionRequestBody, err error) error {
	var refusal *api.APIError
	if !errors.As(err, &refusal) {
		return err
	}
	command, lookupErr := closeShowCommand(ctx, store, issue)
	if lookupErr != nil {
		return internalAPIError(lookupErr)
	}
	refusal.Hint = command
	switch refusal.Code {
	case "confirmation_required":
		refusal.Hint += "\nPost a completion claim comment, ask another teammate (or author) to confirm it, then cite that claim with --confirmed."
	case "open_refutations":
		refusal.Hint += "\nResolve the dispute by superseding the claim or refutation, or by rebutting the refutation."
		if refs, ok := refusal.Data["refutations"].([]db.OpenRefutation); ok && ValidateCloseInput(body.Reason, body.Message, body.Evidence) == nil {
			var retry strings.Builder
			retry.WriteString("kata close " + strings.TrimPrefix(command, "kata show ") + " --reason " + shellHintArg(body.Reason) + " --message " + shellHintArg(body.Message))
			if body.DryRun {
				retry.WriteString(" --dry")
			}
			for _, e := range body.Evidence {
				values := []string{}
				switch e.Type {
				case api.EvidenceCommit:
					values = []string{e.SHA}
				case api.EvidencePR:
					values = []string{e.URL}
				case api.EvidenceTest:
					values = []string{e.Command}
				case api.EvidenceReviewedPaths:
					values = e.Paths
				case api.EvidenceExternal:
					values = []string{e.Account}
				case api.EvidenceNoChangeAudit:
					values = []string{e.Rationale}
				case api.EvidenceConfirmed, api.EvidenceRefuted:
					values = []string{e.Comment}
				case api.EvidenceDuplicateOf, api.EvidenceSupersededBy:
					values = []string{e.IssueRef}
				}
				for _, value := range values {
					retry.WriteString(" --evidence " + shellHintArg(string(e.Type)+":"+value))
				}
			}
			for _, ref := range body.Overrule {
				retry.WriteString(" --overrule " + shellHintArg(ref))
			}
			for _, ref := range refs {
				retry.WriteString(" --overrule " + shellHintArg(ref.Refutation))
			}
			refusal.Hint += "\nTo deliberately overrule the listed disputes while keeping this message and evidence:\n" + retry.String()
		}
	}
	return refusal
}
