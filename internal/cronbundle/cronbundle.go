// Package cronbundle shares portable cron collection and import orchestration between clients.
package cronbundle

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Daemon preserves raw API documents and the calling client's authority.
type Daemon interface {
	Read(ctx context.Context, projectID int64, kind, uid string) (int, []byte, error)
	Put(ctx context.Context, projectID int64, kind, uid string, body PutBody) (int, []byte, error)
}

// Project identifies archive provenance or an import destination.
type Project struct {
	ID        int64
	UID, Name string
}

// PutBody carries a complete definition and optional compare-and-swap revision.
type PutBody struct {
	Actor      string         `json:"actor"`
	Name       string         `json:"name"`
	Definition jsontext.Value `json:"definition"`
	Expected   string         `json:"expected_event_uid,omitempty"`
}

// Selection chooses live definitions and their workflow dependencies.
type Selection struct {
	Jobs, Workflows []string
	All             bool
}

// Normalize validates and canonicalizes full UID selectors in place.
func (s *Selection) Normalize() error {
	if s.All && len(s.Jobs)+len(s.Workflows) > 0 || !s.All && len(s.Jobs)+len(s.Workflows) == 0 {
		return validationError("select --job and/or --workflow, or --all alone")
	}
	for _, list := range [][]string{s.Jobs, s.Workflows} {
		for i := range list {
			id := strings.ToUpper(strings.TrimSpace(list[i]))
			if !uid.Valid(id) {
				return validationError("cron identity must be a 26-character ULID")
			}
			list[i] = id
		}
	}
	return nil
}

// ImportOptions controls replacement, prediction and write attribution.
type ImportOptions struct {
	Replace, DryRun bool
	Actor           string
}

// ValidationError identifies a rejected bundle operation.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string   { return e.Message }
func validationError(message string) error { return &ValidationError{Message: message} }

// ResponseError retains daemon response evidence for client-specific errors.
type ResponseError struct {
	Status int
	Body   []byte
}

func (e *ResponseError) Error() string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &envelope) == nil && envelope.Error.Code != "" && envelope.Error.Message != "" {
		return envelope.Error.Code + ": " + envelope.Error.Message
	}
	return fmt.Sprintf("daemon request failed (HTTP %d %s)", e.Status, http.StatusText(e.Status))
}
func responseError(status int, raw []byte) error {
	if status >= 300 && status < 400 {
		return validationError("cron bundle requests do not accept redirects")
	}
	if status >= 400 {
		return &ResponseError{Status: status, Body: raw}
	}
	return nil
}

type record struct {
	db.CronDefinition
	Definition jsontext.Value `json:"definition"`
}

func read(ctx context.Context, a Daemon, projectID int64, kind, operation, id string) ([]record, error) {
	status, raw, err := a.Read(ctx, projectID, kind, id)
	if err != nil {
		return nil, err
	}
	if err := responseError(status, raw); err != nil {
		return nil, err
	}

	var response struct {
		Jobs      []record `json:"jobs"`
		Workflows []record `json:"workflows"`
		Job       record   `json:"job"`
		Workflow  record   `json:"workflow"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if operation == "show" {
		if kind == "job" {
			return []record{response.Job}, nil
		}
		return []record{response.Workflow}, nil
	}
	if kind == "job" {
		return response.Jobs, nil
	}
	return response.Workflows, nil
}

// Export collects selected definitions and their workflow closure in stable order.
func Export(ctx context.Context, a Daemon, project Project, selection Selection) (cron.Bundle, error) {
	bundle := cron.Bundle{Format: "kata.cron.bundle", Version: 1, Source: cron.BundleSource{ProjectUID: project.UID, ProjectName: project.Name, ExportedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Workflows: []cron.BundleDefinition{}, Jobs: []cron.BundleDefinition{}}
	jobs, workflows := map[string]cron.BundleDefinition{}, map[string]cron.BundleDefinition{}
	add := func(kind string, records []record) error {
		for _, record := range records {
			if record.DeletedAt != nil {
				return validationError(fmt.Sprintf("cannot export deleted %s %s", kind, record.UID))
			}
			entry := cron.BundleDefinition{UID: record.UID, DefinitionEventUID: record.DefinitionEventUID, Name: record.Name, Definition: record.Definition}
			if kind == "job" {
				jobs[entry.UID] = entry
			} else {
				workflows[entry.UID] = entry
			}
			if len(jobs)+len(workflows) > 1000 {
				return validationError("bundle exceeds 1000 definitions")
			}
		}
		return nil
	}
	for _, group := range []struct {
		kind string
		ids  []string
	}{{"workflow", selection.Workflows}, {"job", selection.Jobs}} {
		if selection.All {
			records, err := read(ctx, a, project.ID, group.kind, "list", "")
			if err != nil {
				return bundle, err
			}
			if err := add(group.kind, records); err != nil {
				return bundle, err
			}
		} else {
			seen := map[string]bool{}
			for _, id := range group.ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				records, err := read(ctx, a, project.ID, group.kind, "show", id)
				if err != nil {
					return bundle, err
				}
				if err := add(group.kind, records); err != nil {
					return bundle, err
				}
			}
		}
	}
	// Sort before dependency reads as well, so the first reported failure is stable.
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		job, err := cron.DecodeJob(jobs[id].Definition)
		if err != nil {
			return bundle, err
		}
		ref := job.Action.WorkflowUID
		if ref == "" {
			continue
		}
		if _, ok := workflows[ref]; ok {
			continue
		}
		records, err := read(ctx, a, project.ID, "workflow", "show", ref)
		if err != nil {
			return bundle, err
		}
		if err := add("workflow", records); err != nil {
			return bundle, err
		}
	}
	for _, entry := range workflows {
		bundle.Workflows = append(bundle.Workflows, entry)
	}
	for _, entry := range jobs {
		bundle.Jobs = append(bundle.Jobs, entry)
	}
	for _, entries := range [][]cron.BundleDefinition{bundle.Workflows, bundle.Jobs} {
		slices.SortFunc(entries, func(a, b cron.BundleDefinition) int { return strings.Compare(a.UID, b.UID) })
	}
	return bundle, bundle.Validate()
}

// Item reports one attempted definition in import order.
type Item struct {
	Kind      string `json:"kind"`
	SourceUID string `json:"source_uid"`
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
}

// Report retains completed items when an import stops.
type Report struct {
	Target struct {
		UID  string `json:"project_uid"`
		Name string `json:"project_name"`
	} `json:"target"`
	Items  []Item              `json:"items"`
	Review []cron.BundleReview `json:"review"`
	DryRun bool                `json:"dry_run"`
	Note   string              `json:"note,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// Import writes workflows before jobs and stops at the first unresolved result.
func Import(ctx context.Context, a Daemon, project Project, plan cron.ImportPlan, o ImportOptions) (Report, error) {
	report := Report{Items: []Item{}, Review: plan.Review, DryRun: o.DryRun}
	report.Target.UID = project.UID
	report.Target.Name = project.Name
	if o.DryRun {
		report.Note = "Predicted from current reads; spoke projections may be stale. Authorization, live dependencies, lifecycle and CAS are checked on writes."
	}
	for _, group := range []struct {
		kind    string
		entries []cron.PlannedDefinition
	}{{"workflow", plan.Workflows}, {"job", plan.Jobs}} {
		for _, entry := range group.entries {
			outcome, writeErr := o.importItem(ctx, a, project, group.kind, entry, o.Actor)
			report.Items = append(report.Items, Item{group.kind, entry.SourceUID, entry.UID, entry.Name, outcome})
			if writeErr != nil {
				report.Error = writeErr.Error()
				return report, writeErr
			}
		}
	}
	return report, nil
}

// DefinitionEqual ignores object key order while preserving numeric tokens and array order.
func DefinitionEqual(a, b jsontext.Value) (bool, error) {
	left, err := json.Marshal(a, jsontext.ReorderRawObjects(true))
	if err != nil {
		return false, err
	}
	right, err := json.Marshal(b, jsontext.ReorderRawObjects(true))
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func (o ImportOptions) importItem(ctx context.Context, a Daemon, project Project, kind string, entry cron.PlannedDefinition, actor string) (string, error) {
	var current record
	if o.DryRun {
		status, raw, err := a.Read(ctx, project.ID, kind, entry.UID)
		if err != nil {
			return "failed", err
		}
		if status == 404 {
			return "created", nil
		}
		if err := responseError(status, raw); err != nil {
			return "failed", err
		}
		var envelope map[string]jsontext.Value
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "failed", err
		}
		if err := json.Unmarshal(envelope[kind], &current); err != nil {
			return "failed", err
		}
	} else {
		status, raw, err := put(ctx, a, project, kind, entry, actor, "")
		if err != nil {
			return "uncertain", err
		}
		if status >= 200 && status < 300 {
			if err := cronConfirmBundleWrite(status, raw, project, kind, entry); err != nil {
				return "uncertain", err
			}
			return "created", nil
		}
		if status != 409 {
			return cronImportErrorOutcome(status), responseError(status, raw)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
				Data struct {
					Current jsontext.Value `json:"current"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "failed", err
		}
		if envelope.Error.Code != "cron_conflict" {
			return "failed", responseError(status, raw)
		}
		if err := json.Unmarshal(envelope.Error.Data.Current, &current); err != nil {
			return "conflict", validationError("conflict response has no authoritative current definition")
		}
	}
	if err := cronValidateBundleCurrent(current, project, kind, entry.UID); err != nil {
		return "conflict", err
	}
	if current.DeletedAt != nil {
		return "tombstoned", validationError("target " + kind + " is tombstoned; restore it explicitly")
	}

	equal, err := DefinitionEqual(current.Definition, entry.Definition)
	if err != nil {
		return "conflict", err
	}
	if strings.TrimSpace(current.Name) == entry.Name && equal {
		return "unchanged", nil
	}
	if !o.Replace {
		return "conflict", validationError("target " + kind + " differs; use --replace to update it")
	}
	if o.DryRun {
		return "replaced", nil
	}
	status, raw, err := put(ctx, a, project, kind, entry, actor, current.DefinitionEventUID)
	if err != nil {
		return "uncertain", err
	}
	if err := responseError(status, raw); err != nil {
		return cronImportErrorOutcome(status), err
	}
	if err := cronConfirmBundleWrite(status, raw, project, kind, entry); err != nil {
		return "uncertain", err
	}
	return "replaced", nil
}
func cronImportErrorOutcome(status int) string {
	if status >= 500 {
		return "uncertain"
	}
	if status == 409 {
		return "conflict"
	}
	return "failed"
}
func put(ctx context.Context, a Daemon, project Project, kind string, entry cron.PlannedDefinition, actor, expected string) (int, []byte, error) {
	return a.Put(ctx, project.ID, kind, entry.UID, PutBody{Actor: actor, Name: entry.Name, Definition: entry.Definition, Expected: expected})
}

// A historical current document need not pass today's admission rules, but it
// must contain the complete API shape for its resource before it can authorize CAS.
func cronValidateBundleCurrent(current record, project Project, kind, expectedUID string) error {
	if current.UID != expectedUID || current.ProjectID != project.ID || !uid.Valid(current.DefinitionEventUID) || current.DefinitionEventUID != strings.ToUpper(current.DefinitionEventUID) {
		return fmt.Errorf("current %s identity does not match destination", kind)
	}
	// Tombstones never authorize replacement, even when their historical document
	// cannot be decoded by this client.
	if current.DeletedAt != nil {
		return nil
	}
	if name := strings.TrimSpace(current.Name); name == "" || len(name) > 256 {
		return fmt.Errorf("current %s name is missing or invalid", kind)
	}
	if current.Definition.Kind() != '{' {
		return fmt.Errorf("current %s definition must be an object", kind)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(current.Definition, &fields); err != nil {
		return err
	}
	required := map[string]jsontext.Kind{"version": '0', "steps": '['}
	if kind == "job" {
		required = map[string]jsontext.Kind{"version": '0', "kind": '"', "trigger": '{', "action": '{', "overlap": '"', "catchup": '"'}
	}
	for field, want := range required {
		if fields[field].Kind() != want {
			return fmt.Errorf("current %s definition is missing valid %s", kind, field)
		}
	}
	if kind == "job" {
		enabled := fields["enabled"].Kind()
		if enabled != 't' && enabled != 'f' {
			return fmt.Errorf("current job definition is missing enabled")
		}
		for _, field := range []string{"trigger", "action", "issue"} {
			raw, present := fields[field]
			if !present && field == "issue" {
				continue
			}
			var object map[string]jsontext.Value
			if raw.Kind() != '{' || json.Unmarshal(raw, &object) != nil || object["kind"].Kind() != '"' {
				return fmt.Errorf("current job definition has incomplete %s", field)
			}
		}
		job, err := cron.DecodeJob(current.Definition)
		if err != nil {
			return err
		}
		if job.Kind != "job" {
			return fmt.Errorf("current definition has wrong job kind")
		}
	} else {
		_, err := cron.DecodeWorkflow(current.Definition)
		if err != nil {
			return err
		}
		var steps []map[string]jsontext.Value
		if err := json.Unmarshal(fields["steps"], &steps); err != nil {
			return err
		}
		for i, step := range steps {
			if step["key"].Kind() != '"' || step["kind"].Kind() != '"' {
				return fmt.Errorf("current workflow definition has incomplete step %d", i)
			}
		}
	}
	return nil
}

func cronConfirmBundleWrite(status int, raw []byte, project Project, kind string, entry cron.PlannedDefinition) error {
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("unexpected cron write status %d; outcome is uncertain", status)
	}
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("invalid cron write response; outcome is uncertain: %w", err)
	}
	var current record
	if err := json.Unmarshal(envelope[kind], &current); err != nil {
		return fmt.Errorf("incomplete cron write response; outcome is uncertain: %w", err)
	}
	if err := cronValidateBundleCurrent(current, project, kind, entry.UID); err != nil {
		return fmt.Errorf("incomplete cron write response; outcome is uncertain: %w", err)
	}
	if current.DeletedAt != nil {
		return fmt.Errorf("cron write returned a tombstone; outcome is uncertain")
	}
	equal, err := DefinitionEqual(current.Definition, entry.Definition)
	if err != nil {
		return err
	}
	if strings.TrimSpace(current.Name) != entry.Name || !equal {
		return fmt.Errorf("cron write returned different content; outcome is uncertain")
	}
	return nil
}
