package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

type cronReceiptWarning struct {
	Source  string `json:"source"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type cronReceiptFire struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}

type cronReceiptPreview struct {
	Status   string            `json:"status"`
	Basis    string            `json:"basis,omitempty"`
	Delivery string            `json:"delivery,omitempty"`
	Timezone string            `json:"timezone,omitempty"`
	From     *time.Time        `json:"from,omitempty"`
	Next     []cronReceiptFire `json:"next"`
}

type cronReceiptInventory struct {
	Status         string `json:"status"`
	Basis          string `json:"basis,omitempty"`
	InventoryState string `json:"inventory_state,omitempty"`
}

type cronReceipt struct {
	Version     int            `json:"receipt_version"`
	Action      string         `json:"action"`
	Persisted   jsontext.Value `json:"persisted"`
	Diagnostics struct {
		AsOf             time.Time            `json:"as_of"`
		Preview          cronReceiptPreview   `json:"preview"`
		RuntimeInventory cronReceiptInventory `json:"runtime_inventory"`
	} `json:"diagnostics"`
	Warnings []cronReceiptWarning `json:"warnings"`
}

func (r *cronReceipt) warn(source, code, message string) {
	// Server messages may contain user-controlled values. Keep JSON strings
	// bounded without splitting UTF-8; they are never repeated on stderr.
	bound := func(value string) string {
		if len(value) <= 1024 {
			return value
		}
		value = value[:1024]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		return value
	}
	r.Warnings = append(r.Warnings, cronReceiptWarning{source, bound(code), bound(message)})
}

// buildCronReceipt runs only after the mutation response has been received.
// Diagnostic failure cannot turn that successful response into a write retry.
func buildCronReceipt(ctx context.Context, daemon daemonAPI, projectID int64, action string, raw []byte, quiet bool) cronReceipt {
	r := cronReceipt{Version: 1, Action: action, Persisted: bytes.Clone(raw), Warnings: []cronReceiptWarning{}}
	r.Diagnostics.AsOf = time.Now().UTC()
	r.Diagnostics.Preview = cronReceiptPreview{Status: "skipped", Next: []cronReceiptFire{}}
	r.Diagnostics.RuntimeInventory.Status = "skipped"
	if quiet {
		return r
	}
	var response struct {
		Job struct {
			UID                string         `json:"uid"`
			Name               string         `json:"name"`
			DefinitionEventUID string         `json:"definition_event_uid"`
			Definition         jsontext.Value `json:"definition"`
			DeletedAt          *time.Time     `json:"deleted_at"`
		} `json:"job"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || !uid.Valid(response.Job.UID) || !uid.Valid(response.Job.DefinitionEventUID) {
		r.Diagnostics.Preview.Status, r.Diagnostics.RuntimeInventory.Status = "unavailable", "unavailable"
		r.warn("receipt", "response_unknown", "Saved response retained; diagnostics need a recognized job identity and revision.")
		return r
	}
	job, err := cron.ParseJob(response.Job.Definition)
	if err != nil {
		r.Diagnostics.Preview.Status, r.Diagnostics.RuntimeInventory.Status = "unavailable", "unavailable"
		r.warn("receipt", "definition_unknown", "Saved response retained; this client cannot interpret its job definition.")
		return r
	}
	if !job.Enabled || response.Job.DeletedAt != nil {
		r.warn("receipt", "inactive", "No upcoming dispatch is shown for a paused or deleted job.")
		return r
	}
	// One shared budget bounds both observations, including body reads. Keep
	// the caller's cancellation; diagnostics do not outlive the command.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body, err := json.Marshal(struct {
		Name             string         `json:"name"`
		Definition       jsontext.Value `json:"definition"`
		UID              string         `json:"uid"`
		ExpectedEventUID string         `json:"expected_event_uid"`
	}{response.Job.Name, response.Job.Definition, response.Job.UID, response.Job.DefinitionEventUID})
	if err != nil {
		r.Diagnostics.Preview.Status = "unavailable"
		r.warn("preview", "request_unknown", "Saved response retained; preview input could not be encoded.")
	} else {
		query := url.Values{"count": {"3"}, "from": {r.Diagnostics.AsOf.Format(time.RFC3339Nano)}}
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate?%s", projectID, query.Encode())
		status, data := cronReceiptRequest(ctx, daemon, http.MethodPost, path, body)
		r.Diagnostics.Preview.Status = status
		if status == "available" {
			r.readPreview(data, cron.IsRecurringIssueJob(job))
		} else {
			r.warn("preview", status, "Job saved; schedule diagnostics are "+status+".")
		}
	}
	if cron.IsRecurringIssueJob(job) || job.Action.Kind != "execute" {
		r.Diagnostics.RuntimeInventory.Status = "not_applicable"
		return r
	}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", projectID)
	status, data := cronReceiptRequest(ctx, daemon, http.MethodGet, path, nil)
	r.Diagnostics.RuntimeInventory.Status = status
	if status == "available" {
		r.readInventory(data, response.Job.UID)
	} else {
		r.warn("runtime_inventory", status, "Job saved; runtime inventory is "+status+".")
	}
	return r
}

// cronReceiptRequest uses the already resolved authenticated transport. Refuse
// redirects even to the same origin: a diagnostic POST must stay on its route.
func cronReceiptRequest(ctx context.Context, daemon daemonAPI, method, path string, body []byte) (string, []byte) {
	request, err := http.NewRequestWithContext(ctx, method, daemon.baseURL+path, bytes.NewReader(body))
	if err != nil || daemon.client == nil {
		return "unavailable", nil
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *daemon.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "unavailable", nil
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return "unsupported", nil
	case http.StatusOK:
	default:
		return "unavailable", nil
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) > limit {
		return "unavailable", nil
	}
	return "available", data
}

// These private adapters recognize additive versions of the diagnostic wire
// contracts. Missing required members never become an available empty result.
func (r *cronReceipt) readPreview(data []byte, native bool) {
	var value struct {
		Valid  *bool `json:"valid"`
		Errors []struct {
			Stage   *string `json:"stage"`
			Code    *string `json:"code"`
			Message *string `json:"message"`
		} `json:"errors"`
		Warnings []struct {
			Code    *string `json:"code"`
			Message *string `json:"message"`
		} `json:"warnings"`
		Schedule *struct {
			Delivery string            `json:"delivery"`
			Basis    string            `json:"basis"`
			Zone     string            `json:"zone"`
			From     *time.Time        `json:"from"`
			Next     []cronReceiptFire `json:"next"`
		} `json:"schedule"`
	}
	fail := func() {
		r.Diagnostics.Preview.Status = "unavailable"
		r.warn("preview", "response_unknown", "Job saved; schedule diagnostic is incomplete, stale or not understood.")
	}
	if err := json.Unmarshal(data, &value); err != nil || value.Valid == nil || value.Errors == nil || value.Warnings == nil {
		fail()
		return
	}
	validMessages := true
	for _, e := range value.Errors {
		if e.Stage == nil || e.Code == nil || e.Message == nil || *e.Stage == "" || *e.Code == "" {
			validMessages = false
			continue
		}
		r.warn("preview", *e.Code, *e.Stage+": "+*e.Message)
	}
	for _, w := range value.Warnings {
		if w.Code == nil || w.Message == nil || *w.Code == "" {
			validMessages = false
			continue
		}
		r.warn("preview", *w.Code, *w.Message)
	}
	if !validMessages {
		fail()
		return
	}
	if !*value.Valid || len(value.Errors) != 0 || value.Schedule == nil {
		fail()
		return
	}
	for _, w := range value.Warnings {
		if *w.Code == "parent_closed" || *w.Code == "paused" {
			r.Diagnostics.Preview.Status = "skipped"
			return
		}
	}
	s := value.Schedule
	if s.From == nil || !s.From.Equal(r.Diagnostics.AsOf) || strings.TrimSpace(s.Zone) == "" || s.Next == nil || len(s.Next) > 3 {
		fail()
		return
	}
	known := native && s.Delivery == "daemon" && s.Basis == "stored_progress" || !native && s.Delivery == "adapter" && s.Basis == "adapter_trigger"
	if !known {
		fail()
		return
	}
	location, err := time.LoadLocation(s.Zone)
	if err != nil || s.Zone == "Local" {
		fail()
		return
	}
	previous := r.Diagnostics.AsOf
	for i, fire := range s.Next {
		local, err := time.Parse(time.RFC3339Nano, fire.Local)
		_, offset := local.Zone()
		_, expectedOffset := fire.At.In(location).Zone()
		if err != nil || fire.At.IsZero() || !local.Equal(fire.At) || offset != expectedOffset || fire.At.Before(r.Diagnostics.AsOf) || i > 0 && !fire.At.After(previous) {
			fail()
			return
		}
		s.Next[i].At = fire.At.UTC()
		previous = fire.At
	}
	r.Diagnostics.Preview = cronReceiptPreview{Status: "available", Basis: s.Basis, Delivery: s.Delivery, Timezone: s.Zone, From: s.From, Next: s.Next}
}

func (r *cronReceipt) readInventory(data []byte, jobUID string) {
	var value struct {
		Inventory *struct {
			State string `json:"state"`
		} `json:"inventory"`
		Warnings []jsontext.Value `json:"warnings"`
	}
	if err := json.Unmarshal(data, &value); err != nil || value.Inventory == nil || value.Warnings == nil || value.Inventory.State != "unknown" && value.Inventory.State != "warming" && value.Inventory.State != "ready" {
		r.Diagnostics.RuntimeInventory.Status = "unavailable"
		r.warn("runtime_inventory", "response_unknown", "Job saved; runtime inventory is incomplete or not understood.")
		return
	}
	validMessages := true
	revisionCaveat := false
	for _, raw := range value.Warnings {
		// Decode entries independently so a wrong-typed sibling cannot erase
		// a recognized advisory. The enclosing inventory still fails closed.
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || fields["code"].Kind() != '"' || fields["message"].Kind() != '"' {
			validMessages = false
			continue
		}
		if rawUID, present := fields["job_uid"]; present && rawUID.Kind() != '"' {
			validMessages = false
			continue
		}
		code, codeErr := cronStringField(fields, "code")
		message, messageErr := cronStringField(fields, "message")
		warningJobUID, uidErr := cronStringField(fields, "job_uid")
		if codeErr != nil || messageErr != nil || uidErr != nil || code == "" {
			validMessages = false
			continue
		}
		if value.Inventory.State != "ready" {
			continue
		}
		relevant := code == "no_live_runtime" && warningJobUID == ""
		if warningJobUID == jobUID {
			relevant = code == "pin_unmatched" || code == "adapter_unmatched" || code == "choice_unknown" && r.Action != "restore"
			revisionCaveat = revisionCaveat || relevant
		}
		if relevant {
			r.warn("runtime_inventory", code, message)
		}
	}
	if revisionCaveat {
		r.warn("runtime_inventory", "revision_unbound", "Inventory describes subsequent project state; the same job UID may have a different definition revision.")
	}
	if !validMessages {
		r.Diagnostics.RuntimeInventory.Status = "unavailable"
		r.warn("runtime_inventory", "response_unknown", "Job saved; runtime warning data is incomplete or not understood.")
		return
	}
	r.Diagnostics.RuntimeInventory = cronReceiptInventory{Status: "available", Basis: "subsequent_project_inventory", InventoryState: value.Inventory.State}
}
