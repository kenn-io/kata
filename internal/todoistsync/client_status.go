package todoistsync

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync/todoistapi"
)

// StatusSession reads task status and writes it with Todoist's close and reopen operations.
type StatusSession interface {
	ReadStatus(context.Context, Config, StatusTarget) (issuesync.StatusObservation, error)
	WriteStatus(ctx context.Context, c Config, t StatusTarget, desired string, admit func() error) (issuesync.StatusObservation, error)
}

// StatusTarget names one mapped task and its last verified status observation.
type StatusTarget struct {
	ID    string
	Prior *db.IssueStatusObservation
}

// historyOverlap matches the cursor overlap used by the other issue sync providers.
const historyOverlap = 2 * time.Minute

// activeTask reads one task. Todoist serves only active tasks here, so found
// is false for completed tasks. A task Todoist returns deleted or in another
// project is out of scope and blocks instead of reading as missing.
func (s *clientSession) activeTask(ctx context.Context, c Config, id string) (Task, bool, error) {
	resp, err := s.api.GetTaskAPIV1TasksTaskIDGetWithResponse(ctx, &todoistapi.GetTaskAPIV1TasksTaskIDGetRequestOptions{
		PathParams: &todoistapi.GetTaskAPIV1TasksTaskIDGetPath{TaskID: url.PathEscape(id)},
	})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return Task{}, false, nil
	}
	if err := responseError(err, "read Todoist task"); err != nil {
		return Task{}, false, err
	}
	if resp == nil || resp.JSON200 == nil || len(resp.Body) == 0 {
		return Task{}, false, emptyResponse("read Todoist task")
	}
	t := taskFrom(*resp.JSON200, c.historyFloor())
	if t.Deleted || t.ProjectID != c.ProjectID {
		return Task{}, false, blocked("Todoist task was deleted or moved out of the selected project")
	}
	return t, true, nil
}

// completedTask finds a task that is no longer active in completion history,
// starting just before its last open observation. It never infers completion
// from disappearance.
func (s *clientSession) completedTask(ctx context.Context, c Config, t StatusTarget) (Task, error) {
	var since time.Time
	// Only an open observation bounds the completion. A closed task may have
	// been edited after completion, so its version can follow completed_at.
	if t.Prior != nil && t.Prior.Raw != nil && *t.Prior.Raw == "open" {
		since = t.Prior.Version.Add(-historyOverlap)
	}
	history, err := s.completed(ctx, c, since, time.Now())
	if err != nil {
		return Task{}, err
	}
	for _, row := range mergeTasks(nil, history) {
		if row.ID == t.ID {
			return row, nil
		}
	}
	return Task{}, blocked("Todoist task is missing, deleted, moved or completed before the history floor")
}

func (s *clientSession) currentTask(ctx context.Context, c Config, t StatusTarget) (Task, error) {
	row, found, err := s.activeTask(ctx, c, t.ID)
	if err != nil || found {
		return row, err
	}
	return s.completedTask(ctx, c, t)
}

func observation(row Task) issuesync.StatusObservation {
	raw := "open"
	o := issuesync.StatusObservation{Status: "open", Version: row.UpdatedAt.Truncate(time.Millisecond)}
	if row.Checked {
		raw = "closed"
		o.Status = "closed"
		o.ClosedReason = "done"
		if row.CompletedAt != nil {
			o.ClosedAt = new(row.CompletedAt.Truncate(time.Millisecond))
		}
	}
	o.RawStatus = &raw
	return o
}

// observationAfter keeps a fresh read from going backwards. With a null
// update time, a reopened task falls back to an older timestamp than the
// stored observation, and the status store would reject it.
func observationAfter(row Task, prior *db.IssueStatusObservation) issuesync.StatusObservation {
	o := observation(row)
	if prior == nil || o.Version.After(prior.Version) {
		return o
	}
	o.Version = prior.Version
	if prior.Raw == nil || *prior.Raw != *o.RawStatus {
		o.Version = prior.Version.Add(time.Millisecond)
	}
	return o
}

// ReadStatus reports a task's current status. A task last seen closed and
// still not active stays closed without another history scan.
func (s *clientSession) ReadStatus(ctx context.Context, c Config, t StatusTarget) (issuesync.StatusObservation, error) {
	row, found, err := s.activeTask(ctx, c, t.ID)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	if !found {
		if t.Prior != nil && t.Prior.Raw != nil && *t.Prior.Raw == "closed" {
			raw := "closed"
			return issuesync.StatusObservation{RawStatus: &raw, Status: "closed", ClosedReason: "done", Version: t.Prior.Version}, nil
		}
		if row, err = s.completedTask(ctx, c, t); err != nil {
			return issuesync.StatusObservation{}, err
		}
	}
	return observationAfter(row, t.Prior), nil
}

// WriteStatus closes or reopens one task. It refuses writes that Todoist
// applies to more than this task: closing a task with subtasks completes them,
// closing a recurring task moves it to its next date, and reopening restores
// the task's parent tasks and archived section. A lost or unverified response
// stays ambiguous; the next attempt starts with reads.
func (s *clientSession) WriteStatus(ctx context.Context, c Config, t StatusTarget, desired string, admit func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	row, err := s.currentTask(ctx, c, t)
	if err != nil {
		return zero, err
	}
	observed := observationAfter(row, t.Prior)
	if observed.Status == desired {
		return observed, nil
	}
	if row.Recurring {
		return zero, blocked("Todoist recurring tasks move to their next date when closed; change them in Todoist")
	}
	if desired == "closed" {
		children, err := s.activeTasks(ctx, c, t.ID)
		if err != nil {
			return zero, err
		}
		if len(children) > 0 {
			return zero, blocked("Todoist close would complete subtasks; complete this task in Todoist")
		}
	} else {
		if row.ParentID != "" {
			return zero, blocked("Todoist reopen would restore parent tasks; reopen this task in Todoist")
		}
		if row.SectionID != "" {
			if err := s.activeSection(ctx, row.SectionID); err != nil {
				return zero, err
			}
		}
	}
	if err := admit(); err != nil {
		return zero, err
	}
	traced, sent := issuesync.TrackRequestWrite(ctx)
	var resp *http.Response
	if desired == "closed" {
		r, callErr := s.api.CloseTaskAPIV1TasksTaskIDClosePostWithResponse(traced, &todoistapi.CloseTaskAPIV1TasksTaskIDClosePostRequestOptions{
			PathParams: &todoistapi.CloseTaskAPIV1TasksTaskIDClosePostPath{TaskID: url.PathEscape(t.ID)},
		})
		if r != nil {
			resp = r.HTTPResponse
		}
		err = callErr
	} else {
		r, callErr := s.api.ReopenTaskAPIV1TasksTaskIDReopenPostWithResponse(traced, &todoistapi.ReopenTaskAPIV1TasksTaskIDReopenPostRequestOptions{
			PathParams: &todoistapi.ReopenTaskAPIV1TasksTaskIDReopenPostPath{TaskID: url.PathEscape(t.ID)},
		})
		if r != nil {
			resp = r.HTTPResponse
		}
		err = callErr
	}
	if err != nil {
		return zero, writeError(resp, sent())
	}
	before := &db.IssueStatusObservation{Raw: observed.RawStatus, Version: observed.Version}
	verified, err := s.ReadStatus(ctx, c, StatusTarget{ID: t.ID, Prior: before})
	if err != nil || verified.Status != desired {
		return zero, &issuesync.StatusError{Message: "Todoist status write could not be verified", Ambiguous: true}
	}
	return verified, nil
}

// writeError classifies a failed close or reopen. Without a response, the
// write is ambiguous once its request was sent.
func writeError(resp *http.Response, sent bool) error {
	if resp == nil {
		return &issuesync.StatusError{Message: "Todoist status write response was lost", Ambiguous: sent}
	}
	code := resp.StatusCode
	return &issuesync.StatusError{
		Message:    fmt.Sprintf("Todoist status write failed (HTTP %d)", code),
		HTTPStatus: code,
		Ambiguous:  code >= 500,
		Blocked:    code >= 300 && code < 500 && code != http.StatusConflict && code != http.StatusTooManyRequests,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}
}

// activeSection refuses a reopen that would restore an archived section.
func (s *clientSession) activeSection(ctx context.Context, id string) error {
	resp, err := s.api.GetSectionAPIV1SectionsSectionIDGetWithResponse(ctx, &todoistapi.GetSectionAPIV1SectionsSectionIDGetRequestOptions{
		PathParams: &todoistapi.GetSectionAPIV1SectionsSectionIDGetPath{SectionID: url.PathEscape(id)},
	})
	if err := responseError(err, "read Todoist section"); err != nil {
		return err
	}
	if resp == nil || resp.JSON200 == nil || len(resp.Body) == 0 {
		return emptyResponse("read Todoist section")
	}
	if resp.JSON200.IsArchived || resp.JSON200.IsDeleted {
		return blocked("Todoist reopen would restore an archived section; reopen this task in Todoist")
	}
	return nil
}

var _ StatusSession = (*clientSession)(nil)
