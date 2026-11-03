package recurrence

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/client/generated"
)

// This file is what every client of the daemon's recurrence routes shares:
// `kata schedule --repeat` and the TUI's recurrence form build the template
// from the issue, pick the zone, refuse a cron parent and guard writes the
// same way. The daemon is the only validator of a rule.

// CronProgressPrefix starts the metadata keys a native cron job writes on the
// issue it parents.
const CronProgressPrefix = "cron.progress."

// CronParentRefusal and CronParentHint are the refusal for an issue that
// parents a native recurring job: closing it as done would pause the job.
const (
	CronParentRefusal = "this issue parents a recurring cron job; closing it as done would pause the job"
	CronParentHint    = "make the work a child instead: kata cron job create --parent <issue-ref>"
)

// ParentsCronJob reports whether the issue metadata marks a parent of a
// native recurring cron job.
func ParentsCronJob[V any](metadata map[string]V) bool {
	for key := range metadata {
		if strings.HasPrefix(key, CronProgressPrefix) {
			return true
		}
	}
	return false
}

// View is a daemon recurrence row as clients read it, and the block
// `kata show --json` adds under "recurrence". NextOccurrenceKey is the date
// after the issue's own occurrence; it is null once the rule is exhausted.
type View struct {
	ID                int64   `json:"id"`
	UID               string  `json:"uid"`
	RRule             string  `json:"rrule"`
	DTStart           string  `json:"dtstart"`
	Timezone          string  `json:"timezone"`
	NextOccurrenceKey *string `json:"next_occurrence_key"`
	Revision          int64   `json:"revision"`
}

// Zone is the recurrence's zone, or "" when there is no recurrence yet.
func (r *View) Zone() string {
	if r == nil {
		return ""
	}
	return r.Timezone
}

// Terms is what a start or a change writes.
type Terms struct {
	Rule, Start, Zone, Actor string
}

// Template is the issue a recurrence is built from: its title, body,
// priority and labels. Owner and metadata are left out on purpose.
type Template struct {
	Title    string
	Body     string
	Priority *int64
	Labels   []string
}

// ETag is the If-Match value for a revision.
func ETag(revision int64) string { return fmt.Sprintf(`"rev-%d"`, revision) }

// Errors ResolveZone returns when no usable zone exists.
var (
	ErrLocalZone = errors.New(`"Local" is not an IANA zone`)
	ErrNoZone    = errors.New("no timezone known for this recurrence")
)

// ResolveZone picks the zone for a recurrence: the caller's explicit zone,
// then the zone the recurrence already has, then the issue's timezone
// metadata, then the caller's own zone. A change must not fall back to the
// issue's metadata before the recurrence's zone: the PATCH leaves the issue's
// metadata alone, so it goes stale after the first change. "Local" is never a
// zone the daemon can resolve the same way on every host, so it is never
// returned.
func ResolveZone(explicit, recurrenceZone, issueZone string, localZone func() string) (string, error) {
	for _, zone := range []string{strings.TrimSpace(explicit), recurrenceZone, issueZone} {
		if zone == "" {
			continue
		}
		if zone == "Local" {
			return "", ErrLocalZone
		}
		return zone, nil
	}
	if zone := localZone(); zone != "" {
		return zone, nil
	}
	return "", ErrNoZone
}

// DefaultLocaltimePath is the system zone link.
const DefaultLocaltimePath = "/etc/localtime"

// LocalZone names the caller's local IANA zone: TZ first, then the
// /etc/localtime link. It returns "" when nothing names a zone, so the
// daemon falls back to its own timezone.
func LocalZone() string {
	tz, set := os.LookupEnv("TZ")
	return ResolveLocalZone(tz, set, DefaultLocaltimePath)
}

// ResolveLocalZone is LocalZone with its inputs given. A TZ that is set but
// empty means UTC, as in POSIX and Go.
func ResolveLocalZone(tz string, tzSet bool, localtimePath string) string {
	if tzSet && strings.TrimSpace(tz) == "" {
		return "UTC"
	}
	if name := strings.TrimPrefix(strings.TrimSpace(tz), ":"); isIANAZone(name) {
		return name
	}
	target, err := os.Readlink(localtimePath)
	if err != nil {
		return ""
	}
	// Windows stores link targets with backslashes.
	_, name, found := strings.CutLast(filepath.ToSlash(target), "zoneinfo/")
	if found && isIANAZone(name) {
		return name
	}
	return ""
}

// isIANAZone reports whether name loads as a zone. "Local" is excluded: it
// means the daemon's zone, not the caller's.
func isIANAZone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// StatusError is a daemon answer with a status of 400 or more.
type StatusError struct {
	Status      int
	Body        []byte
	Method, URI string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URI, e.Status)
}

// ErrNoResponse is returned when a request produced neither a response nor an
// error.
var ErrNoResponse = errors.New("recurrence request returned no response")

// noResponse is the error for a call that returned no response.
func noResponse(callErr error) error {
	if callErr != nil {
		return callErr
	}
	return ErrNoResponse
}

// answer turns a response into an error: a *StatusError for a refusal, the
// call's own error (a decode failure) otherwise, nil for a success.
func answer(status int, body []byte, httpResp *http.Response, callErr error) error {
	if status < http.StatusBadRequest {
		return callErr
	}
	e := &StatusError{Status: status, Body: body}
	if httpResp != nil && httpResp.Request != nil {
		e.Method, e.URI = httpResp.Request.Method, httpResp.Request.URL.RequestURI()
	}
	return e
}

// List reads the project's live recurrences.
func List(ctx context.Context, api *generated.Client, projectID int64) ([]View, error) {
	response, callErr := api.ListRecurrencesWithResponse(ctx, &generated.ListRecurrencesRequestOptions{
		PathParams: &generated.ListRecurrencesPath{ProjectID: projectID},
	})
	if response == nil {
		return nil, noResponse(callErr)
	}
	if err := answer(response.StatusCode, response.Body, response.HTTPResponse, callErr); err != nil {
		return nil, err
	}
	var body struct {
		Recurrences []View `json:"recurrences"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return nil, err
	}
	return body.Recurrences, nil
}

// Active finds the live recurrence the issue is an occurrence of. The show
// response carries only its numeric id, so the project's live recurrences are
// matched by it; a stopped one is absent from the list.
func Active(ctx context.Context, api *generated.Client, projectID int64, id *int64) (*View, error) {
	if id == nil {
		return nil, nil
	}
	recs, err := List(ctx, api, projectID)
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

// Start creates a recurrence from the issue with ref as its first
// occurrence. A non-nil ifMatch makes the daemon re-check the issue's
// revision inside the transaction that moves its date.
func Start(ctx context.Context, api *generated.Client, projectID int64, ref string, tpl Template, terms Terms, ifMatch *string) error {
	body := tpl.Body
	response, callErr := api.CreateRecurrenceWithResponse(ctx, &generated.CreateRecurrenceRequestOptions{
		PathParams: &generated.CreateRecurrencePath{ProjectID: projectID},
		Header:     &generated.CreateRecurrenceHeaders{IfMatch: ifMatch},
		Body: &generated.CreateRecurrenceBody{
			Actor: &terms.Actor, Rrule: terms.Rule, Dtstart: terms.Start, Timezone: terms.Zone, InitialIssueRef: &ref,
			Template: generated.RecurrenceTemplateInput{
				Title: tpl.Title, Body: &body, Priority: tpl.Priority, Labels: tpl.Labels,
			},
		},
	})
	if response == nil {
		return noResponse(callErr)
	}
	return answer(response.StatusCode, response.Body, response.HTTPResponse, callErr)
}

// Change rewrites the rule, start and zone of current, guarded on the
// revision current carries.
func Change(ctx context.Context, api *generated.Client, projectID int64, current View, terms Terms) error {
	tag := ETag(current.Revision)
	response, callErr := api.PatchRecurrenceWithResponse(ctx, &generated.PatchRecurrenceRequestOptions{
		PathParams: &generated.PatchRecurrencePath{ProjectID: projectID, RecurrenceUID: current.UID},
		Header:     &generated.PatchRecurrenceHeaders{IfMatch: &tag},
		Body:       &generated.PatchRecurrenceBody{Actor: &terms.Actor, Rrule: &terms.Rule, Dtstart: &terms.Start, Timezone: &terms.Zone},
	})
	if response == nil {
		return noResponse(callErr)
	}
	return answer(response.StatusCode, response.Body, response.HTTPResponse, callErr)
}

// Stop ends current, guarded on the revision current carries. Occurrences
// already created stay.
func Stop(ctx context.Context, api *generated.Client, projectID int64, current View, actor string) error {
	response, callErr := api.DeleteRecurrenceWithResponse(ctx, &generated.DeleteRecurrenceRequestOptions{
		PathParams: &generated.DeleteRecurrencePath{ProjectID: projectID, RecurrenceUID: current.UID},
		Query:      &generated.DeleteRecurrenceQuery{Actor: &actor},
		Header:     &generated.DeleteRecurrenceHeaders{IfMatch: ETag(current.Revision)},
	})
	if response == nil {
		return noResponse(callErr)
	}
	return answer(response.StatusCode, response.Body, response.HTTPResponse, callErr)
}
