package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
)

func projectedCloseWarnings(event *db.Event) []db.VerificationWarning {
	if event == nil {
		return nil
	}
	var payload struct {
		Warnings []db.VerificationWarning `json:"verification_warnings"`
	}
	_ = json.Unmarshal([]byte(event.Payload), &payload)
	return payload.Warnings
}

func projectedCloseEndpointProjects(event *db.Event) map[string]string {
	if event == nil {
		return nil
	}
	var payload struct {
		EndpointProjects map[string]string `json:"verification_endpoint_projects"`
	}
	_ = json.Unmarshal([]byte(event.Payload), &payload)
	return payload.EndpointProjects
}

// projectCloseEvent releases retained references only through the reader's
// current comment authority. The stored event stays canonical for replication
// and exact retry; display handles are recomputed for each authorized view.
func projectCloseEvent(ctx context.Context, store db.Storage, event *db.Event) (*db.Event, error) {
	if event == nil || event.Type != "issue.closed" || event.Payload == "" {
		return event, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(event.Payload), &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return event, nil
	}
	var evidence []db.Evidence
	var warnings []db.VerificationWarning
	var overruled []string
	if raw := fields["evidence"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return nil, err
		}
	}
	if raw := fields["verification_warnings"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &warnings); err != nil {
			return nil, err
		}
	}
	if raw := fields["overruled_refutations"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &overruled); err != nil {
			return nil, err
		}
	}
	refs := []string{}
	for _, e := range evidence {
		if e.Type == "confirmed" || e.Type == "refuted" {
			refs = append(refs, e.Comment)
		}
	}
	for _, w := range warnings {
		for _, r := range w.Refutations {
			refs = append(refs, r.Refutation, r.Target)
		}
	}
	refs = append(refs, overruled...)
	if len(refs) == 0 {
		return event, nil
	}
	subjectProject := event.ProjectUID
	var subject db.Issue
	if event.IssueUID != nil {
		var err error
		subject, err = store.IssueByUID(ctx, *event.IssueUID, db.IncludeDeletedYes)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			subjectProject = subject.ProjectUID
		}
	}
	ids, err := store.CommentIssueIDsByUIDs(ctx, refs)
	if err != nil {
		return nil, err
	}
	visible := map[string]commentref.Record{}
	loaded := map[int64]bool{}
	for _, id := range ids {
		issue, err := store.IssueByID(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if issue.DeletedAt != nil {
			continue
		}
		if err := authorizeIssueScopedIssue(ctx, store, issue); err != nil {
			continue
		}
		if err := authorizeCommentEndpoint(ctx, issue.ProjectID); err != nil {
			continue
		}
		if loaded[issue.ProjectID] {
			continue
		}
		loaded[issue.ProjectID] = true
		records, err := readCommentRecords(ctx, store, db.CommentGraphQuery{ProjectID: issue.ProjectID})
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			visible[r.UID] = r
		}
	}
	_, restricted := db.AuthorizedProjects(ctx)
	fallback := !restricted && issueScopeFromContext(ctx) == nil
	verificationIssueUIDs := slices.Clone(event.VerificationIssueUIDs)
	// Preserve the current endpoint owner for clients with a fixed startup
	// scope; an unqualified comment handle cannot identify its project.
	verificationEndpointProjects := map[string]string{}
	render := func(uid string) (string, bool) {
		r, ok := visible[uid]
		if !ok {
			return uid, fallback
		}
		db.RecordIssueScopeTarget(ctx, r.IssueID)
		if !slices.Contains(verificationIssueUIDs, r.IssueUID) {
			verificationIssueUIDs = append(verificationIssueUIDs, r.IssueUID)
		}
		shown := ""
		if event.IssueUID != nil {
			shown = *event.IssueUID
		}
		handle := commentref.HandleForIssue(r.Handle, r.IssueUID, r.IssueShortID, shown)
		if r.ProjectUID != subjectProject {
			handle = r.ProjectName + "#" + handle
		}
		verificationEndpointProjects[handle] = r.ProjectUID
		return handle, true
	}
	keptEvidence := []db.Evidence{}
	for _, e := range evidence {
		if e.Type == "confirmed" || e.Type == "refuted" {
			value, ok := render(e.Comment)
			if !ok {
				continue
			}
			e.Comment = value
		}
		keptEvidence = append(keptEvidence, e)
	}
	keptWarnings := []db.VerificationWarning{}
	for _, w := range warnings {
		kept := []db.OpenRefutation{}
		for _, r := range w.Refutations {
			ref, ok := render(r.Refutation)
			target, targetOK := render(r.Target)
			if !ok || !targetOK {
				continue
			}
			r.Refutation, r.Target = ref, target
			kept = append(kept, r)
		}
		if len(kept) > 0 {
			w.Refutations = kept
			if subject.UID != "" {
				w.Hint, err = closeShowCommand(ctx, store, subject)
				if err != nil {
					return nil, err
				}
			}

			keptWarnings = append(keptWarnings, w)
		}
	}
	keptOverrules := []string{}
	for _, ref := range overruled {
		if handle, ok := render(ref); ok {
			keptOverrules = append(keptOverrules, handle)
		}
	}
	for key, value := range map[string]any{"evidence": keptEvidence, "verification_warnings": keptWarnings, "overruled_refutations": keptOverrules} {
		delete(fields, key)
		size := 0
		switch v := value.(type) {
		case []db.Evidence:
			size = len(v)
		case []db.VerificationWarning:
			size = len(v)
		case []string:
			size = len(v)
		}
		if size > 0 {
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			fields[key] = raw
		}
	}
	delete(fields, "verification_endpoint_projects")
	if len(verificationEndpointProjects) > 0 {
		projectJSON, err := json.Marshal(verificationEndpointProjects, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		fields["verification_endpoint_projects"] = projectJSON
	}
	delete(fields, "verification_issue_uids")
	raw, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	projected := *event
	projected.Payload = string(raw)
	projected.ContentHash = ""
	projected.VerificationIssueUIDs = verificationIssueUIDs
	return &projected, nil
}
