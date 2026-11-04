package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
)

// repeatAction is what `kata schedule --repeat` did; it is the JSON "action".
type repeatAction string

const (
	repeatStarted repeatAction = "started"
	repeatChanged repeatAction = "changed"
	repeatStopped repeatAction = "stopped"
)

// recurrenceView is a daemon recurrence row as the CLI reads it, and the
// block `kata show --json` adds under "recurrence".
type recurrenceView = recurrenceclient.View

func recurrenceLine(r *recurrenceView) string {
	next := "none"
	if r.NextOccurrenceKey != nil {
		next = textsafe.Line(*r.NextOccurrenceKey)
	}
	return fmt.Sprintf("repeat: %s, start %s, timezone %s, next %s",
		textsafe.Line(r.RRule), textsafe.Line(r.DTStart), textsafe.Line(r.Timezone), next)
}

type scheduleRepeatResult struct {
	Ref         string       `json:"ref"`
	Action      repeatAction `json:"action"`
	ScheduledOn string       `json:"scheduled_on"`
	// Timezone is the issue's own zone. A change leaves it alone, so on a
	// change it can differ from the recurrence's zone.
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

func errNoActiveRecurrence() error {
	return &cliError{Message: "issue has no active recurrence; nothing was written", Kind: kindNotFound, ExitCode: ExitNotFound}
}

func errRevisionConflict(message string) error {
	return &cliError{HTTPStatus: http.StatusPreconditionFailed, Message: message, Kind: kindConfirm, ExitCode: ExitConfirm}
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
	issue, showBody, err := fetchMetaIssue(ctx, httpClient, baseURL, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if recurrenceclient.ParentsCronJob(issue.Metadata) {
		return &cliError{
			Message: recurrenceclient.CronParentRefusal,
			Hint:    recurrenceclient.CronParentHint,
			Kind:    kindValidation, ExitCode: ExitValidation,
		}
	}
	// A historical ref resolves an issue that has since moved; every
	// recurrence call belongs to the project the issue is in now, under the
	// short id it has there.
	requestedPID := pid
	pid, ref.RefForAPI = issue.ProjectID, issue.ShortID
	if err := checkIssueIfMatch(opts.IfMatch, issue.Revision); err != nil {
		return err
	}
	guardStart := !stop && strings.TrimSpace(opts.IfMatch) != ""
	actor, _ := resolveActor(ctx, flags.As, nil)

	current, err := recurrenceclient.Active(ctx, api.Client, pid, issue.RecurrenceID)
	if err != nil {
		return recurrenceCLIError(err)
	}
	var terms recurrenceclient.Terms
	if !stop {
		zone, err := repeatZone(opts.Timezone, current.Zone(), issue.Metadata)
		if err != nil {
			return err
		}
		terms = recurrenceclient.Terms{Rule: opts.Rule, Start: args[1], Zone: zone, Actor: actor}
	}
	action := repeatStarted
	switch {
	case stop:
		if current == nil {
			return errNoActiveRecurrence()
		}
		if err := stopRecurrence(ctx, api, pid, *current, actor); err != nil {
			return err
		}
		action = repeatStopped
	case current != nil:
		if err := changeRecurrence(ctx, api, pid, *current, terms); err != nil {
			return err
		}
		action = repeatChanged
	default:
		// Starting is the one write that changes the issue (its date moves),
		// so the daemon re-checks the revision inside that transaction; an
		// older daemon would ignore the header and could overwrite a
		// competing write. A change uses the recurrence's own guarded PATCH.
		var ifMatch *string
		if guardStart {
			if err := requireDaemonAPIVersion(ctx, httpClient, baseURL, apiVersionRecurrenceIfMatch, "--if-match when starting a recurrence"); err != nil {
				return err
			}
			tag := recurrenceclient.ETag(issue.Revision)
			ifMatch = &tag
		}
		tpl, err := recurrenceclient.TemplateFromShow(showBody)
		if err != nil {
			return err
		}
		if err := startRecurrence(ctx, api, pid, ref.RefForAPI, tpl, terms, ifMatch); err != nil {
			return err
		}
	}

	after, _, err := fetchMetaIssue(ctx, httpClient, baseURL, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	displayRef, err := issueRefForOutput(daemonAPI{ctx: ctx, baseURL: baseURL, client: httpClient}, requestedPID, ref.ProjectName,
		issueIdentityForOutput{ProjectID: issue.ProjectID, UID: after.UID, ShortID: after.ShortID})
	if err != nil {
		return err
	}
	result := scheduleRepeatResult{
		Ref:         displayRef,
		Action:      action,
		ScheduledOn: metadataString(after.Metadata, "scheduled_on"),
	}
	result.Timezone = metadataString(after.Metadata, "timezone")
	if !stop {
		result.Recurrence, _ = recurrenceclient.Active(ctx, api.Client, pid, after.RecurrenceID)
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
	if etag != recurrenceclient.ETag(revision) {
		return errRevisionConflict(fmt.Sprintf("revision conflict: issue revision is %d, not %s; nothing was written", revision, strings.Trim(etag, `"`)))
	}
	return nil
}

// repeatZone picks the zone for the recurrence: --timezone, then the zone the
// recurrence already has, then the issue's timezone metadata, then the
// caller's own zone.
func repeatZone(flagZone, recurrenceZone string, metadata map[string]jsontext.Value) (string, error) {
	zone, err := recurrenceclient.ResolveZone(flagZone, recurrenceZone, metadataString(metadata, "timezone"), localTimeZoneName)
	switch {
	case errors.Is(err, recurrenceclient.ErrLocalZone):
		return "", &cliError{Message: `"Local" is not an IANA zone; pass --timezone with a name like Europe/Paris`, Kind: kindValidation, ExitCode: ExitValidation}
	case errors.Is(err, recurrenceclient.ErrNoZone):
		return "", &cliError{
			Message: "no timezone known for this recurrence; pass --timezone with an IANA name like Europe/Paris",
			Kind:    kindValidation, ExitCode: ExitValidation,
		}
	}
	return zone, err
}

// recurrenceCLIError maps what the shared recurrence calls return to the
// errors the CLI prints for a daemon answer.
func recurrenceCLIError(err error) error {
	if status, ok := errors.AsType[*recurrenceclient.StatusError](err); ok {
		return externalCLIResponseError(status.Status, status.Body, nil)
	}
	if errors.Is(err, recurrenceclient.ErrNoResponse) {
		return externalCLITransportError[struct{}](nil, nil)
	}
	return err
}

// recurrenceStatus is the daemon status of err when it is a refusal, else 0.
func recurrenceStatus(err error) int {
	if status, ok := errors.AsType[*recurrenceclient.StatusError](err); ok {
		return status.Status
	}
	return 0
}

func startRecurrence(ctx context.Context, api *kataclient.Client, pid int64, ref string, tpl recurrenceclient.Template, terms recurrenceclient.Terms, ifMatch *string) error {
	err := recurrenceclient.Start(ctx, api.Client, pid, ref, tpl, terms, ifMatch)
	if recurrenceStatus(err) == http.StatusPreconditionFailed {
		return errRevisionConflict("revision conflict: the issue changed since it was read; nothing was written")
	}
	return recurrenceCLIError(err)
}

func changeRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceView, terms recurrenceclient.Terms) error {
	err := recurrenceclient.Change(ctx, api.Client, pid, current, terms)
	if recurrenceStatus(err) == http.StatusPreconditionFailed {
		return recurrenceConflict(ctx, api, pid, current.UID)
	}
	return recurrenceCLIError(err)
}

func stopRecurrence(ctx context.Context, api *kataclient.Client, pid int64, current recurrenceView, actor string) error {
	err := recurrenceclient.Stop(ctx, api.Client, pid, current, actor)
	switch recurrenceStatus(err) {
	case http.StatusPreconditionFailed:
		return recurrenceConflict(ctx, api, pid, current.UID)
	case http.StatusNotFound:
		return errNoActiveRecurrence()
	}
	return recurrenceCLIError(err)
}

// recurrenceConflict reports a write that lost to a concurrent one, with the
// recurrence as it stands now so the caller can decide again.
func recurrenceConflict(ctx context.Context, api *kataclient.Client, pid int64, uid string) error {
	message := "revision conflict: the recurrence changed since it was read; nothing was written"
	recs, err := recurrenceclient.List(ctx, api.Client, pid)
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
			agentRowField("repeat", string(r.Action)),
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
	if r.Action == repeatChanged {
		// The change moves the recurrence only; the issue keeps the date and
		// zone it had, so say so rather than print them as if they were new.
		if r.Recurrence != nil {
			if _, err := fmt.Fprintln(out, recurrenceLine(r.Recurrence)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "scheduled_on: %s (this issue's date did not move)\n", textsafe.Line(r.ScheduledOn)); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "timezone: %s (this issue's zone did not change)\n", textsafe.Line(r.Timezone))
		return err
	}
	if _, err := fmt.Fprintf(out, "scheduled_on: %s\ntimezone: %s\n", textsafe.Line(r.ScheduledOn), textsafe.Line(r.Timezone)); err != nil {
		return err
	}
	if r.Recurrence == nil {
		return nil
	}
	_, err := fmt.Fprintln(out, recurrenceLine(r.Recurrence))
	return err
}

// showRecurrenceFor reads the recurrence of the issue in a show response. It
// is best effort: a token that may not list recurrences still gets the issue.
func showRecurrenceFor(ctx context.Context, api *kataclient.Client, showBody []byte) *recurrenceView {
	var shown metaShowResponse
	if json.Unmarshal(showBody, &shown) != nil {
		return nil
	}
	rec, err := recurrenceclient.Active(ctx, api.Client, shown.Issue.ProjectID, shown.Issue.RecurrenceID)
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
