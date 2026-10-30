package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronParentPrefix = "cron.progress."

// recurrenceView is a daemon recurrence row as the CLI reads it, and the
// block `kata show --json` adds under "recurrence". NextOccurrenceKey is the
// date after the issue's own occurrence; it is null once the rule is
// exhausted.
type recurrenceView struct {
	ID                int64   `json:"id"`
	UID               string  `json:"uid"`
	RRule             string  `json:"rrule"`
	DTStart           string  `json:"dtstart"`
	Timezone          string  `json:"timezone"`
	NextOccurrenceKey *string `json:"next_occurrence_key"`
	Revision          int64   `json:"revision"`
}

// zone is the recurrence's zone, or "" when there is no recurrence yet.
func (r *recurrenceView) zone() string {
	if r == nil {
		return ""
	}
	return r.Timezone
}

func (r *recurrenceView) line() string {
	next := "none"
	if r.NextOccurrenceKey != nil {
		next = textsafe.Line(*r.NextOccurrenceKey)
	}
	return fmt.Sprintf("repeat: %s, start %s, timezone %s, next %s",
		textsafe.Line(r.RRule), textsafe.Line(r.DTStart), textsafe.Line(r.Timezone), next)
}

// recurrenceTerms is what a start or a change writes.
type recurrenceTerms struct {
	Rule, Start, Zone, Actor string
}

type scheduleRepeatResult struct {
	Ref         string `json:"ref"`
	Action      string `json:"action"`
	ScheduledOn string `json:"scheduled_on"`
	// Timezone is the issue's own zone. A change leaves it alone, so it is not
	// reported there: the recurrence's zone is the one that changed.
	Timezone string `json:"timezone,omitempty"`
	// Recurrence is the rule as it stands after a start or change; absent after a stop.
	Recurrence *recurrenceView `json:"recurrence,omitempty"`
}

// scheduleRepeatOptions carries the flags of `kata schedule` that only apply
// to a recurrence.
type scheduleRepeatOptions struct {
	Rule     string
	Timezone string
	IfMatch  string
}

func scheduleRepeatUsage(message string) error {
	return &cliError{Message: message, Kind: kindUsage, ExitCode: ExitUsage}
}

func errNoActiveRecurrence(status int) error {
	return &cliError{HTTPStatus: status, Message: "issue has no active recurrence; nothing was written", Kind: kindNotFound, ExitCode: ExitNotFound}
}

func errRevisionConflict(message string) error {
	return &cliError{HTTPStatus: http.StatusPreconditionFailed, Message: message, Kind: kindConfirm, ExitCode: ExitConfirm}
}

func revisionETag(rev int64) string { return fmt.Sprintf(`"rev-%d"`, rev) }

func runScheduleRepeat(cmd *cobra.Command, args []string, opts scheduleRepeatOptions) error {
	stop := opts.Rule == "-"
	switch {
	case stop && len(args) != 1:
		return scheduleRepeatUsage("kata schedule <issue-ref> --repeat - takes no date")
	case stop && opts.Timezone != "":
		return scheduleRepeatUsage("--timezone does not apply when stopping a recurrence")
	case !stop && len(args) != 2:
		return scheduleRepeatUsage("--repeat <rrule> needs the start date: kata schedule <issue-ref> <date> --repeat <rrule>")
	case strings.TrimSpace(opts.Rule) == "":
		return scheduleRepeatUsage("--repeat must not be blank; use --repeat - to stop a recurrence")
	}

	ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
	if err != nil {
		return err
	}
	httpClient, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	api, err := kataclient.NewWithHTTPClient(baseURL, httpClient)
	if err != nil {
		return err
	}
	issue, _, err := fetchMetaIssue(ctx, httpClient, baseURL, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	for key := range issue.Metadata {
		if strings.HasPrefix(key, cronParentPrefix) {
			return &cliError{
				Message: "this issue parents a recurring cron job; closing it as done would pause the job",
				Hint:    "make the work a child instead: kata cron job create --parent <issue-ref>",
				Kind:    kindValidation, ExitCode: ExitValidation,
			}
		}
	}
	// A historical ref resolves an issue that has since moved; every
	// recurrence call belongs to the project the issue is in now, under the
	// short id it has there.
	pid, ref.RefForAPI = issue.ProjectID, issue.ShortID
	if err := checkIssueIfMatch(opts.IfMatch, issue.Revision); err != nil {
		return err
	}
	actor, _ := resolveActor(ctx, flags.As, nil)

	current, err := activeRecurrenceOf(ctx, api, pid, issue.RecurrenceID)
	if err != nil {
		return err
	}
	var terms recurrenceTerms
	if !stop {
		zone, err := repeatZone(opts.Timezone, current.zone(), issue.Metadata)
		if err != nil {
			return err
		}
		terms = recurrenceTerms{Rule: opts.Rule, Start: args[1], Zone: zone, Actor: actor}
	}
	action := "started"
	switch {
	case stop:
		if current == nil {
			return errNoActiveRecurrence(0)
		}
		if err := stopRecurrence(ctx, api, pid, *current, actor); err != nil {
			return err
		}
		action = "stopped"
	case current != nil:
		if err := changeRecurrence(ctx, api, pid, *current, terms); err != nil {
			return err
		}
		action = "changed"
	default:
		if err := startRecurrence(ctx, api, pid, ref.RefForAPI, issue, terms); err != nil {
			return err
		}
	}

	after, _, err := fetchMetaIssue(ctx, httpClient, baseURL, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	result := scheduleRepeatResult{
		Ref:         after.ShortID,
		Action:      action,
		ScheduledOn: metadataString(after.Metadata, "scheduled_on"),
	}
	if action != "changed" {
		result.Timezone = metadataString(after.Metadata, "timezone")
	}
	if !stop {
		result.Recurrence, _ = activeRecurrenceOf(ctx, api, pid, after.RecurrenceID)
	}
	return printScheduleRepeat(cmd, result)
}

func metadataString(md map[string]jsontext.Value, key string) string {
	var s string
	if raw, ok := md[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// checkIssueIfMatch applies --if-match to the issue revision before anything
// is written, the way the metadata patch behind plain `kata schedule` does.
func checkIssueIfMatch(raw string, revision int64) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	etag, err := normalizeMetaIfMatch(raw)
	if err != nil {
		return err
	}
	if etag != revisionETag(revision) {
		return errRevisionConflict(fmt.Sprintf("revision conflict: issue revision is %d, not %s; nothing was written", revision, strings.Trim(etag, `"`)))
	}
	return nil
}

// repeatZone picks the zone for the recurrence: --timezone, then the zone the
// recurrence already has, then the issue's timezone metadata, then the
// caller's own zone. A change must not fall back to the issue's metadata
// before the recurrence's zone: the PATCH leaves the issue's metadata alone,
// so it goes stale after the first change. "Local" is never a zone the daemon
// can resolve the same way on every host, so it is never sent.
func repeatZone(flagZone, recurrenceZone string, metadata map[string]jsontext.Value) (string, error) {
	for _, zone := range []string{strings.TrimSpace(flagZone), recurrenceZone, metadataString(metadata, "timezone")} {
		if zone == "" {
			continue
		}
		if zone == "Local" {
			return "", &cliError{Message: `"Local" is not an IANA zone; pass --timezone with a name like Europe/Paris`, Kind: kindValidation, ExitCode: ExitValidation}
		}
		return zone, nil
	}
	if zone := localTimeZoneName(); zone != "" {
		return zone, nil
	}
	return "", &cliError{
		Message: "no timezone known for this recurrence; pass --timezone with an IANA name like Europe/Paris",
		Kind:    kindValidation, ExitCode: ExitValidation,
	}
}

// activeRecurrenceOf finds the live recurrence the issue is an occurrence of.
// The show response carries only its numeric id, so the project's live
// recurrences are matched by it; a stopped one is absent from the list.
func activeRecurrenceOf(ctx context.Context, api *kataclient.Client, pid int64, id *int64) (*recurrenceView, error) {
	if id == nil {
		return nil, nil
	}
	recs, err := listRecurrences(ctx, api, pid)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == *id {
			return &recs[i], nil
		}
	}
	return nil, nil
}

func listRecurrences(ctx context.Context, api *kataclient.Client, pid int64) ([]recurrenceView, error) {
	response, callErr := api.ListRecurrencesWithResponse(ctx, &generated.ListRecurrencesRequestOptions{
		PathParams: &generated.ListRecurrencesPath{ProjectID: pid},
	})
	if response == nil {
		return nil, externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return nil, err
	}
	var body struct {
		Recurrences []recurrenceView `json:"recurrences"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return nil, err
	}
	return body.Recurrences, nil
}

func startRecurrence(ctx context.Context, api *kataclient.Client, pid int64, ref string, issue metaIssueWire, terms recurrenceTerms) error {
	body := issue.Body
	response, callErr := api.CreateRecurrenceWithResponse(ctx, &generated.CreateRecurrenceRequestOptions{
		PathParams: &generated.CreateRecurrencePath{ProjectID: pid},
		Body: &generated.CreateRecurrenceBody{
			Actor: &terms.Actor, Rrule: terms.Rule, Dtstart: terms.Start, Timezone: terms.Zone, InitialIssueRef: &ref,
			Template: generated.RecurrenceTemplateInput{
				Title: issue.Title, Body: &body, Priority: issue.Priority, Labels: issue.Labels,
			},
		},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

func changeRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceView, terms recurrenceTerms) error {
	tag := revisionETag(current.Revision)
	response, callErr := api.PatchRecurrenceWithResponse(ctx, &generated.PatchRecurrenceRequestOptions{
		PathParams: &generated.PatchRecurrencePath{ProjectID: pid, RecurrenceUID: current.UID},
		Header:     &generated.PatchRecurrenceHeaders{IfMatch: &tag},
		Body:       &generated.PatchRecurrenceBody{Actor: &terms.Actor, Rrule: &terms.Rule, Dtstart: &terms.Start, Timezone: &terms.Zone},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if response.StatusCode == http.StatusPreconditionFailed {
		return recurrenceConflict(ctx, api, pid, current.UID)
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

func stopRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceView, actor string) error {
	response, callErr := api.DeleteRecurrenceWithResponse(ctx, &generated.DeleteRecurrenceRequestOptions{
		PathParams: &generated.DeleteRecurrencePath{ProjectID: pid, RecurrenceUID: current.UID},
		Query:      &generated.DeleteRecurrenceQuery{Actor: &actor},
		Header:     &generated.DeleteRecurrenceHeaders{IfMatch: revisionETag(current.Revision)},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if response.StatusCode == http.StatusPreconditionFailed {
		return recurrenceConflict(ctx, api, pid, current.UID)
	}
	if response.StatusCode == http.StatusNotFound {
		return errNoActiveRecurrence(response.StatusCode)
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

// recurrenceConflict reports a write that lost to a concurrent one, with the
// recurrence as it stands now so the caller can decide again.
func recurrenceConflict(ctx context.Context, api *kataclient.Client, pid int64, uid string) error {
	message := "revision conflict: the recurrence changed since it was read; nothing was written"
	recs, err := listRecurrences(ctx, api, pid)
	if err != nil {
		return errRevisionConflict(message)
	}
	for _, rec := range recs {
		if rec.UID == uid {
			return errRevisionConflict(message + fmt.Sprintf(" (it now repeats %s from %s in %s, rev-%d)",
				textsafe.Line(rec.RRule), textsafe.Line(rec.DTStart), textsafe.Line(rec.Timezone), rec.Revision))
		}
	}
	return errRevisionConflict(message + " (the recurrence was stopped meanwhile)")
}

func printScheduleRepeat(cmd *cobra.Command, r scheduleRepeatResult) error {
	out := cmd.OutOrStdout()
	switch currentOutputMode() {
	case outputJSON:
		var buf bytes.Buffer
		if err := emitJSON(&buf, r); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, buf.String())
		return err
	case outputAgent:
		if flags.Quiet {
			return nil
		}
		fields := []agentField{
			agentRowField("ref", r.Ref),
			agentRowField("repeat", r.Action),
			agentRowField("scheduled_on", r.ScheduledOn),
		}
		if r.Timezone != "" {
			fields = append(fields, agentRowField("timezone", r.Timezone))
		}
		if r.Recurrence != nil {
			fields = append(fields,
				agentRowField("rrule", r.Recurrence.RRule),
				agentRowField("start", r.Recurrence.DTStart),
				agentRowField("recurrence_timezone", r.Recurrence.Timezone))
		}
		return writeAgentOKLine(out, "schedule", fields...)
	}
	if flags.Quiet {
		return nil
	}
	if _, err := fmt.Fprintf(out, "%s repeat %s\n", textsafe.Line(r.Ref), r.Action); err != nil {
		return err
	}
	if r.Action == "changed" {
		// The change moves the recurrence only; the issue keeps the date and
		// zone it had, so say so rather than print them as if they were new.
		if r.Recurrence != nil {
			if _, err := fmt.Fprintln(out, r.Recurrence.line()); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(out, "scheduled_on: %s (this issue's date did not move)\n", textsafe.Line(r.ScheduledOn))
		return err
	}
	if _, err := fmt.Fprintf(out, "scheduled_on: %s\ntimezone: %s\n", textsafe.Line(r.ScheduledOn), textsafe.Line(r.Timezone)); err != nil {
		return err
	}
	if r.Recurrence == nil {
		return nil
	}
	_, err := fmt.Fprintln(out, r.Recurrence.line())
	return err
}

// showRecurrenceFor reads the recurrence of the issue in a show response. It
// is best effort: a token that may not list recurrences still gets the issue.
func showRecurrenceFor(ctx context.Context, api *kataclient.Client, showBody []byte) *recurrenceView {
	var shown metaShowResponse
	if json.Unmarshal(showBody, &shown) != nil {
		return nil
	}
	rec, err := activeRecurrenceOf(ctx, api, shown.Issue.ProjectID, shown.Issue.RecurrenceID)
	if err != nil {
		return nil
	}
	return rec
}

// withRecurrence adds the recurrence to a raw show response body.
func withRecurrence(showBody []byte, rec *recurrenceView) ([]byte, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(showBody, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]jsontext.Value{}
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	fields["recurrence"] = encoded
	return json.Marshal(fields, json.Deterministic(true))
}
