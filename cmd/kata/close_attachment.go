package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

const apiVersionAttachmentEvidence = "0.35.0"

// prepareCloseAttachments validates the entire local request before any link.
// Full IDs bypass current metadata lookup so saved close receipts can replay.
func prepareCloseAttachments(cmd *cobra.Command, raw, reason, message string, evidence []api.Evidence, files []string, dry bool) ([]api.Evidence, error) {
	hasAttachment := false
	for _, e := range evidence {
		hasAttachment = hasAttachment || e.Type == api.EvidenceAttachment
	}
	if !hasAttachment && len(files) == 0 {
		return evidence, nil
	}
	if len(files) > 0 && (reason != "done" || dry || cmd.Flags().Changed("if-match") || cmd.Flags().Changed("idempotency-key")) {
		return nil, externalCLIValidation("--docbank requires done and cannot combine with --dry, --if-match or --idempotency-key; link first, then close with attachment:<full-id>")
	}
	inputs := make([]generated.LinkAttachmentBody, len(files))
	for i, file := range files {
		input, err := parseDocbankSelector(file)
		if err != nil {
			return nil, err
		}
		inputs[i] = input
	}
	validation := append([]api.Evidence(nil), evidence...)
	if len(files) > 0 {
		validation = append(validation, api.Evidence{Type: api.EvidenceAttachment, AttachmentID: "pending-link"})
	}
	if err := daemon.ValidateCloseInput(reason, message, validation); err != nil {
		return nil, externalCLIValidation(err.Error())
	}
	if _, _, err := prepareFollowupComment(cmd); err != nil {
		return nil, err
	}
	ctx, base, pid, ref, err := resolveIssueRefForCommand(cmd, raw)
	if err != nil {
		return nil, err
	}
	hc, err := longRunningClientFor(ctx, base)
	if err != nil {
		return nil, err
	}
	if err := requireDaemonAPIVersion(ctx, hc, base, apiVersionAttachmentEvidence, "attachment close evidence"); err != nil {
		return nil, err
	}
	out := append([]api.Evidence(nil), evidence...)
	var ids []string
	loaded := false
	for i, e := range out {
		if e.Type != api.EvidenceAttachment {
			continue
		}
		id := strings.TrimSpace(e.AttachmentID)
		if uid.Valid(strings.ToUpper(id)) {
			out[i].AttachmentID = id
			continue
		}
		if !loaded {
			issue, _, err := fetchMetaIssue(ctx, hc, base, pid, ref.RefForAPI)
			if err != nil {
				return nil, err
			}
			for key, value := range issue.Metadata {
				if id, ok := strings.CutPrefix(key, metadata.AttachmentPrefix); ok {
					if _, valid := metadata.Valid(value); valid {
						ids = append(ids, id)
					}
				}
			}
			loaded = true
		}
		id, err = resolveAttachmentSuffix(ids, id)
		if err != nil {
			return nil, err
		}
		out[i].AttachmentID = id
	}
	if duplicate := findDuplicateEvidence(out); duplicate != "" {
		return nil, externalCLIValidation("duplicate evidence item " + duplicate)
	}
	seen := map[string]bool{}
	for _, e := range out {
		if e.Type == api.EvidenceAttachment {
			seen[e.AttachmentID] = true
		}
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	var linked []string
	for i, input := range inputs {
		input.Actor = &actor
		result, err := linkAttachmentCLI(ctx, hc, base, pid, ref.RefForAPI, input)
		if err != nil {
			return nil, fmt.Errorf("docbank selector %q failed; already linked attachment IDs: [%s]; issue was not closed: %w", files[i], strings.Join(linked, ", "), err)
		}
		linked = append(linked, result.AttachmentID)
		if !seen[result.AttachmentID] {
			seen[result.AttachmentID] = true
			out = append(out, api.Evidence{Type: api.EvidenceAttachment, AttachmentID: result.AttachmentID})
		}
	}
	return out, nil
}
