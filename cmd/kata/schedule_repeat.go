package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronParentPrefix = "cron.progress."

// localtimePath is the link the caller's zone is read from when TZ is unset.
var localtimePath = "/etc/localtime"

// recurrenceWire is the slice of a daemon recurrence row the CLI reads.
type recurrenceWire struct {
	UID               string  `json:"uid"`
	RRule             string  `json:"rrule"`
	DTStart           string  `json:"dtstart"`
	Timezone          string  `json:"timezone"`
	NextOccurrenceKey *string `json:"next_occurrence_key"`
	Revision          int64   `json:"revision"`
}

// repeatIssueWire is the slice of the show response the repeat flow reads.
type repeatIssueWire struct {
	raw   []byte
	Issue struct {
		ShortID      string                    `json:"short_id"`
		Title        string                    `json:"title"`
		Body         string                    `json:"body"`
		Priority     *int64                    `json:"priority"`
		Labels       []string                  `json:"labels"`
		Metadata     map[string]jsontext.Value `json:"metadata"`
		Revision     int64                     `json:"revision"`
		RecurrenceID *int64                    `json:"recurrence_id"`
	} `json:"issue"`
}

type scheduleRepeatResult struct {
	Ref         string `json:"ref"`
	Action      string `json:"action"`
	ScheduledOn string `json:"scheduled_on"`
	Timezone    string `json:"timezone"`
	// Recurrence is the rule as it stands after a start or change; absent after a stop.
	Recurrence *showRecurrence `json:"recurrence,omitempty"`
}

// scheduleRepeatOptions carries the flags of `kata schedule` that only apply
// to a recurrence.
type scheduleRepeatOptions struct {
	Rule     string
	Timezone string
	IfMatch  string
}

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
	issue, err := readRepeatIssue(ctx, api, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	for key := range issue.Issue.Metadata {
		if strings.HasPrefix(key, cronParentPrefix) {
			return &cliError{
				Message: "this issue parents a recurring cron job; closing it as done would pause the job",
				Hint:    "make the work a child instead: kata cron job create --parent <issue-ref>",
				Kind:    kindValidation, ExitCode: ExitValidation,
			}
		}
	}
	if err := checkIssueIfMatch(opts.IfMatch, issue.Issue.Revision); err != nil {
		return err
	}
	actor, _ := resolveActor(ctx, flags.As, nil)

	current, err := activeRecurrenceOf(ctx, api, pid, issue.Issue.RecurrenceID)
	if err != nil {
		return err
	}
	action := "started"
	switch {
	case stop:
		if current == nil {
			return &cliError{Message: "issue has no active recurrence; nothing was written", Kind: kindNotFound, ExitCode: ExitNotFound}
		}
		if err := stopRecurrence(ctx, api, pid, *current, actor); err != nil {
			return err
		}
		action = "stopped"
	case current != nil:
		zone, err := repeatZone(opts.Timezone, issue.Issue.Metadata)
		if err != nil {
			return err
		}
		if err := changeRecurrence(ctx, api, pid, *current, opts.Rule, args[1], zone, actor); err != nil {
			return err
		}
		action = "changed"
	default:
		zone, err := repeatZone(opts.Timezone, issue.Issue.Metadata)
		if err != nil {
			return err
		}
		if err := startRecurrence(ctx, api, pid, ref.RefForAPI, issue, opts.Rule, args[1], zone, actor); err != nil {
			return err
		}
	}

	after, err := readRepeatIssue(ctx, api, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	result := scheduleRepeatResult{
		Ref:         after.Issue.ShortID,
		Action:      action,
		ScheduledOn: metadataString(after.Issue.Metadata, "scheduled_on"),
		Timezone:    metadataString(after.Issue.Metadata, "timezone"),
	}
	if !stop {
		result.Recurrence = showRecurrenceFor(ctx, api, pid, after.raw)
	}
	return printScheduleRepeat(cmd, result)
}

func scheduleRepeatUsage(message string) error {
	return &cliError{Message: message, Kind: kindUsage, ExitCode: ExitUsage}
}

func metadataString(md map[string]jsontext.Value, key string) string {
	var s string
	if raw, ok := md[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func readRepeatIssue(ctx context.Context, api *kataclient.Client, pid int64, ref string) (repeatIssueWire, error) {
	var out repeatIssueWire
	response, callErr := api.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: ref},
	})
	if response == nil {
		return out, externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return out, err
	}
	out.raw = response.Body
	return out, json.Unmarshal(response.Body, &out)
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
	if etag != fmt.Sprintf(`"rev-%d"`, revision) {
		return &cliError{
			HTTPStatus: http.StatusPreconditionFailed,
			Message:    fmt.Sprintf("revision conflict: issue revision is %d, not %s; nothing was written", revision, strings.Trim(etag, `"`)),
			Kind:       kindConfirm, ExitCode: ExitConfirm,
		}
	}
	return nil
}

// repeatZone picks the zone for the recurrence: --timezone, then the issue's
// timezone metadata, then the caller's own zone. "Local" is never a zone the
// daemon can resolve the same way on every host, so it is never sent.
func repeatZone(flagZone string, metadata map[string]jsontext.Value) (string, error) {
	for _, zone := range []string{strings.TrimSpace(flagZone), metadataString(metadata, "timezone")} {
		if zone == "" {
			continue
		}
		if zone == "Local" {
			return "", &cliError{Message: `"Local" is not an IANA zone; pass --timezone with a name like Europe/Paris`, Kind: kindValidation, ExitCode: ExitValidation}
		}
		return zone, nil
	}
	if zone := callerZone(); zone != "" {
		return zone, nil
	}
	return "", &cliError{
		Message: "no timezone known for this recurrence; pass --timezone with an IANA name like Europe/Paris",
		Kind:    kindValidation, ExitCode: ExitValidation,
	}
}

// callerZone is the caller's IANA zone from TZ, else from the zoneinfo path
// /etc/localtime links to. It is empty when neither names a loadable zone.
func callerZone() string {
	if tz := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":"); tz != "" {
		if usableZone(tz) {
			return tz
		}
		return ""
	}
	target, err := os.Readlink(localtimePath)
	if err != nil {
		return ""
	}
	target = filepath.ToSlash(target)
	_, zone, found := strings.Cut(target, "zoneinfo/")
	if !found {
		return ""
	}
	if usableZone(zone) {
		return zone
	}
	return ""
}

func usableZone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// activeRecurrenceOf finds the live recurrence the issue is an occurrence of.
// The show response carries only its numeric id, so the project's live
// recurrences are matched by it; a stopped one is absent from the list.
func activeRecurrenceOf(ctx context.Context, api *kataclient.Client, pid int64, id *int64) (*recurrenceWire, error) {
	if id == nil {
		return nil, nil
	}
	recs, err := listRecurrences(ctx, api, pid)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == *id {
			return &recs[i].recurrenceWire, nil
		}
	}
	return nil, nil
}

type listedRecurrence struct {
	recurrenceWire
	ID int64 `json:"id"`
}

func listRecurrences(ctx context.Context, api *kataclient.Client, pid int64) ([]listedRecurrence, error) {
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
		Recurrences []listedRecurrence `json:"recurrences"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return nil, err
	}
	return body.Recurrences, nil
}

func startRecurrence(ctx context.Context, api *kataclient.Client, pid int64, ref string, issue repeatIssueWire, rule, start, zone, actor string) error {
	body := issue.Issue.Body
	response, callErr := api.CreateRecurrenceWithResponse(ctx, &generated.CreateRecurrenceRequestOptions{
		PathParams: &generated.CreateRecurrencePath{ProjectID: pid},
		Body: &generated.CreateRecurrenceBody{
			Actor: &actor, Rrule: rule, Dtstart: start, Timezone: zone, InitialIssueRef: &ref,
			Template: generated.RecurrenceTemplateInput{
				Title: issue.Issue.Title, Body: &body, Priority: issue.Issue.Priority, Labels: issue.Issue.Labels,
			},
		},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

func recurrenceETag(rev int64) string { return `"rev-` + strconv.FormatInt(rev, 10) + `"` }

func changeRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceWire, rule, start, zone, actor string) error {
	tag := recurrenceETag(current.Revision)
	response, callErr := api.PatchRecurrenceWithResponse(ctx, &generated.PatchRecurrenceRequestOptions{
		PathParams: &generated.PatchRecurrencePath{ProjectID: pid, RecurrenceUID: current.UID},
		Header:     &generated.PatchRecurrenceHeaders{IfMatch: &tag},
		Body:       &generated.PatchRecurrenceBody{Actor: &actor, Rrule: &rule, Dtstart: &start, Timezone: &zone},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if response.StatusCode == http.StatusPreconditionFailed {
		return recurrenceConflict(ctx, api, pid, current.UID)
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

func stopRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceWire, actor string) error {
	response, callErr := api.DeleteRecurrenceWithResponse(ctx, &generated.DeleteRecurrenceRequestOptions{
		PathParams: &generated.DeleteRecurrencePath{ProjectID: pid, RecurrenceUID: current.UID},
		Query:      &generated.DeleteRecurrenceQuery{Actor: &actor},
		Header:     &generated.DeleteRecurrenceHeaders{IfMatch: recurrenceETag(current.Revision)},
	})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if response.StatusCode == http.StatusPreconditionFailed {
		return recurrenceConflict(ctx, api, pid, current.UID)
	}
	if response.StatusCode == http.StatusNotFound {
		return &cliError{Message: "issue has no active recurrence; nothing was written", Kind: kindNotFound, ExitCode: ExitNotFound, HTTPStatus: response.StatusCode}
	}
	return externalCLIResponseError(response.StatusCode, response.Body, callErr)
}

// recurrenceConflict reports a write that lost to a concurrent one, with the
// recurrence as it stands now so the caller can decide again.
func recurrenceConflict(ctx context.Context, api *kataclient.Client, pid int64, uid string) error {
	message := "revision conflict: the recurrence changed since it was read; nothing was written"
	recs, err := listRecurrences(ctx, api, pid)
	if err != nil {
		return &cliError{HTTPStatus: http.StatusPreconditionFailed, Message: message, Kind: kindConfirm, ExitCode: ExitConfirm}
	}
	for _, rec := range recs {
		if rec.UID == uid {
			message += fmt.Sprintf(" (it now repeats %s from %s in %s, rev-%d)",
				textsafe.Line(rec.RRule), textsafe.Line(rec.DTStart), textsafe.Line(rec.Timezone), rec.Revision)
			return &cliError{HTTPStatus: http.StatusPreconditionFailed, Message: message, Kind: kindConfirm, ExitCode: ExitConfirm}
		}
	}
	message += " (the recurrence was stopped meanwhile)"
	return &cliError{HTTPStatus: http.StatusPreconditionFailed, Message: message, Kind: kindConfirm, ExitCode: ExitConfirm}
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
		return writeAgentKVRow(out,
			agentRowField("ref", r.Ref),
			agentRowField("repeat", r.Action),
			agentRowField("scheduled_on", r.ScheduledOn),
			agentRowField("timezone", r.Timezone),
			agentRowField("rrule", r.rule()),
		)
	}
	if flags.Quiet {
		return nil
	}
	if _, err := fmt.Fprintf(out, "%s repeat %s\nscheduled_on: %s\ntimezone: %s\n",
		textsafe.Line(r.Ref), r.Action, textsafe.Line(r.ScheduledOn), textsafe.Line(r.Timezone)); err != nil {
		return err
	}
	if r.Recurrence == nil {
		return nil
	}
	_, err := fmt.Fprintln(out, r.Recurrence.line())
	return err
}

func (r scheduleRepeatResult) rule() string {
	if r.Recurrence == nil {
		return ""
	}
	return r.Recurrence.RRule
}

// showRecurrence is the recurrence block `kata show` adds for an issue that
// belongs to one. NextOccurrence is the date after the issue's own occurrence;
// it is absent once the rule is exhausted.
type showRecurrence struct {
	UID            string  `json:"uid"`
	RRule          string  `json:"rrule"`
	DTStart        string  `json:"dtstart"`
	Timezone       string  `json:"timezone"`
	NextOccurrence *string `json:"next_occurrence"`
	Revision       int64   `json:"revision"`
}

// showRecurrenceFor reads the recurrence of the issue in a show response. It
// is best effort: a token that may not list recurrences still gets the issue.
func showRecurrenceFor(ctx context.Context, api *kataclient.Client, pid int64, showBody []byte) *showRecurrence {
	var issue repeatIssueWire
	if json.Unmarshal(showBody, &issue) != nil {
		return nil
	}
	rec, err := activeRecurrenceOf(ctx, api, pid, issue.Issue.RecurrenceID)
	if err != nil || rec == nil {
		return nil
	}
	return &showRecurrence{
		UID: rec.UID, RRule: rec.RRule, DTStart: rec.DTStart, Timezone: rec.Timezone,
		NextOccurrence: rec.NextOccurrenceKey, Revision: rec.Revision,
	}
}

// withRecurrence adds the recurrence to a raw show response body.
func withRecurrence(showBody []byte, rec *showRecurrence) ([]byte, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(showBody, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	fields["recurrence"] = encoded
	return json.Marshal(fields, json.Deterministic(true))
}

func (r *showRecurrence) line() string {
	next := "none"
	if r.NextOccurrence != nil {
		next = textsafe.Line(*r.NextOccurrence)
	}
	return fmt.Sprintf("repeat: %s, start %s, timezone %s, next %s",
		textsafe.Line(r.RRule), textsafe.Line(r.DTStart), textsafe.Line(r.Timezone), next)
}
