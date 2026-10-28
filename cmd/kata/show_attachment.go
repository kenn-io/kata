package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

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
	var evidence []db.Evidence
	var latest *generated.EventEnvelope
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
			if event.Type != "issue.closed" {
				continue
			}
			if event.IssueUID == nil || *event.IssueUID != ref {
				return nil, fmt.Errorf("issue history close event subject does not match %s", ref)
			}
			var payload struct {
				Evidence []db.Evidence `json:"evidence"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode close history: %w", err)
			}
			if latest == nil || cmp.Or(
				cmp.Compare(event.HlcPhysicalMs, latest.HlcPhysicalMs),
				cmp.Compare(event.HlcCounter, latest.HlcCounter),
				cmp.Compare(event.OriginInstanceUID, latest.OriginInstanceUID),
				cmp.Compare(event.EventUID, latest.EventUID),
			) > 0 {
				latest = &event
				evidence = payload.Evidence
			}
		}
		if len(page.Events) < int(limit) {
			return evidence, nil
		}
		if page.NextAfterID <= after {
			return nil, fmt.Errorf("issue history cursor did not advance")
		}
		after = page.NextAfterID
	}
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
