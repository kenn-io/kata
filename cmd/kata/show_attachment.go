package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// optionalCloseEvidenceForShow leaves ordinary issue display available when
// scoped history is unsupported or the host does not grant history access.
func optionalCloseEvidenceForShow(ctx context.Context, hc *http.Client, base string, pid int64, ref string) ([]db.Evidence, error) {
	if err := requireDaemonAPIVersion(ctx, hc, base, "0.32.0", "issue history"); err != nil {
		return nil, nil
	}
	evidence, err := closeEvidenceForShow(ctx, hc, base, pid, ref)
	var cliErr *cliError
	if errors.As(err, &cliErr) && (cliErr.HTTPStatus == http.StatusForbidden || cliErr.HTTPStatus == http.StatusNotFound) {
		return nil, nil
	}
	return evidence, err
}

// closeEvidenceForShow reads subject history only for human/agent output.
// The JSON show response remains the daemon's unmodified show payload.
func closeEvidenceForShow(ctx context.Context, hc *http.Client, base string, pid int64, ref string) ([]db.Evidence, error) {
	client, err := kataclient.NewWithHTTPClient(base, hc)
	if err != nil {
		return nil, err
	}
	var events []generated.EventEnvelope
	var after int64
	const limit = int64(1000)
	for {
		response, err := client.PollProjectEventsWithResponse(ctx, &generated.PollProjectEventsRequestOptions{PathParams: &generated.PollProjectEventsPath{ProjectID: pid}, Query: &generated.PollProjectEventsQuery{Issue: &ref, AfterID: &after, Limit: new(limit)}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, err); err != nil {
			return nil, err
		}

		if response.Headers200 == nil || response.Headers200.XKataEventsIssueUID != ref {
			return nil, fmt.Errorf("issue history subject acknowledgement does not match %s", ref)
		}
		var page generated.PollEventsBody
		if err := json.Unmarshal(response.Body, &page); err != nil {
			return nil, err
		}
		if page.ResetRequired {
			return nil, fmt.Errorf("issue history requires a reset")
		}
		for _, event := range page.Events {
			switch event.Type {
			case "issue.closed", "issue.reopened", "issue.updated", "issue.created", "issue.snapshot":
				if event.IssueUID == nil || *event.IssueUID != ref {
					return nil, fmt.Errorf("issue history event subject does not match %s", ref)
				}
				events = append(events, event)
			}
		}
		if len(page.Events) < int(limit) {
			return effectiveCloseEvidence(events)
		}
		if page.NextAfterID <= after {
			return nil, fmt.Errorf("issue history cursor did not advance")
		}
		after = page.NextAfterID
	}
}
func effectiveCloseEvidence(events []generated.EventEnvelope) ([]db.Evidence, error) {
	slices.SortStableFunc(events, func(a, b generated.EventEnvelope) int {
		return cmp.Or(cmp.Compare(a.HlcPhysicalMs, b.HlcPhysicalMs),
			cmp.Compare(a.HlcCounter, b.HlcCounter),
			cmp.Compare(a.OriginInstanceUID, b.OriginInstanceUID),
			cmp.Compare(a.EventUID, b.EventUID))
	})
	var evidence []db.Evidence
	var status string
	for _, event := range events {
		switch event.Type {
		case "issue.closed":
			var payload struct {
				Evidence []db.Evidence `json:"evidence"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode close history: %w", err)
			}
			status, evidence = "closed", payload.Evidence
		case "issue.reopened":
			status, evidence = "open", nil
		default:
			var payload struct {
				Status *string `json:"status"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode issue history: %w", err)
			}
			if payload.Status != nil && *payload.Status != status {
				status, evidence = *payload.Status, nil
			}
		}
	}
	return evidence, nil
}

func printShowAttachmentEvidence(out io.Writer, b showResponseForCLI) error {
	for _, e := range b.CloseEvidence {
		if e.Type != "attachment" {
			continue
		}
		current, live := metadata.Valid(b.Issue.Metadata[metadata.AttachmentPrefix+e.AttachmentID])
		descriptor := e.Attachment
		if descriptor == nil && live {
			descriptor = &current
		}
		label := "attachment " + e.AttachmentID + " (removed)"
		if descriptor != nil {
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				return err
			}
			frozen, valid := metadata.Valid(encoded)
			if valid {
				state := ""
				if !live {
					state = " (removed)"
				} else if current.DocbankVersion != frozen.DocbankVersion {
					state = " (changed)"
				}
				label = fmt.Sprintf("attachment %s: %s · %s · %d bytes%s", e.AttachmentID, frozen.Name, frozen.MediaType, frozen.Size, state)
			} else {
				label = "attachment " + e.AttachmentID + " (invalid snapshot)"
			}
		}
		if _, err := fmt.Fprintln(out, "Evidence: "+textsafe.Line(label)); err != nil {
			return err
		}
	}
	return nil
}
