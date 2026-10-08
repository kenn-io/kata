package todoistsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession limits writeback to documented task completion/reopen operations.
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

func isMissing(err error) bool {
	e, ok := errors.AsType[*issuesync.StatusError](err)
	return ok && e.HTTPStatus == 404
}
func (s *clientSession) activeTask(ctx context.Context, c Config, id string) (Task, error) {
	if err := ValidateID(id); err != nil {
		return Task{}, blocked("invalid Todoist task identity")
	}
	raw, err := s.get(ctx, "/api/v1/tasks/"+id)
	if err != nil {
		return Task{}, err
	}
	row, decodeErr := decodeTask(raw)
	if decodeErr != nil || row.ID != id {
		return Task{}, blocked("Todoist task identity does not match")
	}
	if err := validateTask(c, row); err != nil {
		return Task{}, blocked("Todoist task is incomplete, deleted or outside the selected project")
	}
	return row, nil
}

// checkScope re-reads account and project before a write.
func (s *clientSession) checkScope(ctx context.Context, c Config) error {
	if err := s.validate(c); err != nil {
		return err
	}
	if _, err := s.account(ctx); err != nil {
		return err
	}
	_, err := s.Project(ctx, c)
	return err
}

// readScope checks the project once per session; ForRun already pinned the account.
func (s *clientSession) readScope(ctx context.Context, c Config) error {
	if err := s.validate(c); err != nil {
		return err
	}
	if s.scoped {
		return nil
	}
	if _, err := s.Project(ctx, c); err != nil {
		return err
	}
	s.scoped = true
	return nil
}

// completionHistory caches one session's scoped completions and the contiguous
// history interval inspected during targeted lookups.
type completionHistory struct {
	since, until time.Time
	rows         map[string]Task
}

// completedTask searches recent history first and stops as soon as the exact
// task is found. Old mappings can otherwise consume the collection budget
// before a recent completion is reached.
func (s *clientSession) completedTask(ctx context.Context, c Config, id string, since time.Time) (Task, bool, error) {
	floor, err := ParseHistorySince(c.HistorySince)
	if err != nil {
		return Task{}, false, err
	}
	if since.Before(floor) {
		since = floor
	}
	now := s.client.cfg.Now()
	if since.After(now) {
		since = now
	}
	h := s.history
	if h == nil {
		h = &completionHistory{since: now, until: now, rows: map[string]Task{}}
	}
	if row, ok := h.rows[id]; ok && !row.CompletedAt.Before(since) {
		s.history = h
		return row, true, nil
	}
	budget := &readBudget{}
	// Runs normally make these tail reads only minutes apart. Refreshing the
	// whole uncovered tail keeps the cached interval contiguous for later
	// mappings in the same pass.
	if h.until.Before(now) {
		if err := s.extendHistory(ctx, c, h, h.until, now, budget); err != nil {
			s.history = h
			return Task{}, false, err
		}
		h.until = now
	}
	if row, ok := h.rows[id]; ok && !row.CompletedAt.Before(since) {
		s.history = h
		return row, true, nil
	}
	// A negative result is conclusive when the requested range is already in
	// the cached interval. Otherwise walk backward by bounded windows. Each
	// successful window extends the cache, including when a later window fails.
	end := h.since
	for since.Before(end) {
		start := since
		if recentStart := end.Add(-30 * 24 * time.Hour); recentStart.After(start) {
			start = recentStart
		}
		err := s.extendHistory(ctx, c, h, start, end, budget)
		if err != nil {
			s.history = h
			return Task{}, false, err
		}
		h.since = start
		if row, ok := h.rows[id]; ok && !row.CompletedAt.Before(since) {
			s.history = h
			return row, true, nil
		}
		// The page is retained in the cache for other mappings even when it
		// does not contain this target.
		end = start
	}
	s.history = h
	return Task{}, false, nil
}
func (s *clientSession) extendHistory(ctx context.Context, c Config, h *completionHistory, since, until time.Time, budget *readBudget) error {
	page, err := s.completed(ctx, c, since, until, budget)
	if err != nil {
		return err
	}
	merged, err := mergeTasksWithLimit(c, nil, append(slices.Collect(maps.Values(h.rows)), page...), 0)
	if err != nil {
		return err
	}
	for _, row := range merged {
		h.rows[row.ID] = row
	}
	return nil
}

// historyTask resolves a task missing from the active list. It never infers
// completion from disappearance: the exact task must appear in scoped history.
func (s *clientSession) historyTask(ctx context.Context, c Config, t StatusTarget) (Task, error) {
	var since time.Time
	if t.Prior != nil && t.Prior.Raw != nil && *t.Prior.Raw == "open" {
		// The task completed after it was last observed open.
		since = t.Prior.Version.Add(-historyOverlap)
	}
	completed, found, err := s.completedTask(ctx, c, t.ID, since)
	if err != nil {
		return Task{}, err
	}
	// A task can reopen while history is being paged. An active read wins.
	row, err := s.activeTask(ctx, c, t.ID)
	if err == nil || !isMissing(err) {
		return row, err
	}
	if !found {
		return Task{}, blocked("Todoist task is missing, moved, deleted or outside accessible completion history")
	}
	return completed, nil
}

// priorClosed reuses a verified completion for a task that is still not active.
func priorClosed(prior *db.IssueStatusObservation) (issuesync.StatusObservation, bool) {
	if prior == nil || prior.Raw == nil || *prior.Raw != "closed" {
		return issuesync.StatusObservation{}, false
	}
	raw := "closed"
	return issuesync.StatusObservation{RawStatus: &raw, Status: "closed", ClosedReason: "done", Version: prior.Version}, true
}
func observation(row Task, prior *db.IssueStatusObservation) issuesync.StatusObservation {
	raw, status := "open", "open"
	o := issuesync.StatusObservation{Status: status, Version: row.UpdatedAt.UTC().Truncate(time.Millisecond)}
	if *row.Checked {
		raw = "closed"
		o.Status = "closed"
		o.ClosedReason = "done"
		o.ClosedAt = new(row.CompletedAt.UTC().Truncate(time.Millisecond))
	}
	o.RawStatus = &raw
	if !row.updatedAtKnown && prior != nil && !prior.Version.IsZero() && !o.Version.After(prior.Version) {
		if prior.Raw == nil || *prior.Raw != raw {
			o.Version = prior.Version.Add(time.Millisecond)
		} else {
			o.Version = prior.Version
		}
	}
	return o
}
func (s *clientSession) ReadStatus(ctx context.Context, c Config, t StatusTarget) (issuesync.StatusObservation, error) {
	if err := s.readScope(ctx, c); err != nil {
		return issuesync.StatusObservation{}, err
	}
	row, err := s.activeTask(ctx, c, t.ID)
	if isMissing(err) {
		if prior, ok := priorClosed(t.Prior); ok {
			return prior, nil
		}
		row, err = s.historyTask(ctx, c, t)
	}
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return observation(row, t.Prior), nil
}
func (s *clientSession) activeSection(ctx context.Context, c Config, id string) error {
	if err := ValidateID(id); err != nil {
		return blocked("invalid Todoist section identity")
	}
	raw, err := s.get(ctx, "/api/v1/sections/"+id)
	if err != nil {
		return err
	}
	var section struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
		Archived  *bool  `json:"is_archived"`
		Deleted   *bool  `json:"is_deleted"`
	}
	if json.Unmarshal(raw, &section) != nil || section.ID != id || section.ProjectID != c.ProjectID || section.Archived == nil || section.Deleted == nil || *section.Archived || *section.Deleted {
		return blocked("Todoist reopen requires an active section in the selected project")
	}
	return nil
}

// WriteStatus sends one POST after pacing and the durable intent admission fence.
// A lost/unverified response stays ambiguous; the next attempt starts with reads.
func (s *clientSession) WriteStatus(ctx context.Context, c Config, t StatusTarget, desired string, admit func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if c.StatusSync != "two-way" || admit == nil {
		return zero, blocked("Todoist writes require two-way mode and delivery admission")
	}
	if desired != "open" && desired != "closed" {
		return zero, blocked("Todoist status must be open or closed")
	}
	if err := s.checkScope(ctx, c); err != nil {
		return zero, err
	}
	if desired == "open" {
		// Reopen safety depends on the task's current project and hierarchy.
		// Earlier status reads may have cached an older history row.
		s.history = nil
	}
	row, err := s.activeTask(ctx, c, t.ID)
	if isMissing(err) {
		if prior, ok := priorClosed(t.Prior); ok && desired == "closed" {
			return prior, nil
		}
		row, err = s.historyTask(ctx, c, t)
	}
	if err != nil {
		return zero, err
	}
	observed := observation(row, t.Prior)
	if observed.Status == desired {
		return observed, nil
	}
	if !row.recurrenceKnown || !row.hierarchyKnown {
		return zero, blocked("Todoist write requires known task recurrence and hierarchy state")
	}
	if row.Due != nil && row.Due.Recurring {
		return zero, blocked("Todoist recurring tasks require completion in Todoist; writeback is blocked")
	}
	if desired == "closed" {
		children, err := s.taskPages(ctx, c, "/api/v1/tasks", "results", url.Values{"parent_id": {t.ID}}, &readBudget{})
		if err != nil {
			return zero, err
		}
		for _, child := range children {
			if !child.hierarchyKnown {
				return zero, blocked("Todoist completion requires known child task hierarchy")
			}
			if child.ParentID == t.ID {
				return zero, blocked("Todoist close would complete subtasks; complete this task in Todoist")
			}
		}
	} else {
		if row.ParentID != "" {
			return zero, blocked("Todoist reopen would restore ancestor tasks; reopen this task in Todoist")
		}
		if row.SectionID != "" {
			if err := s.activeSection(ctx, c, row.SectionID); err != nil {
				return zero, err
			}
		}
	}
	action := "close"
	if desired == "open" {
		action = "reopen"
	}
	if err := s.client.admit(ctx); err != nil {
		return zero, err
	}
	if err := admit(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	traced, sent := issuesync.TrackRequestWrite(ctx)
	req, err := http.NewRequestWithContext(traced, http.MethodPost, c.APIOrigin+"/api/v1/tasks/"+t.ID+"/"+action, nil)
	if err != nil {
		return zero, blocked("cannot build Todoist status request")
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	// A write can change a task's active/history membership even when its
	// response is lost. Drop the pass snapshot before dispatch so verification
	// must observe fresh provider state.
	s.history = nil
	resp, err := s.client.http.Do(req)
	if err != nil {
		if !sent() {
			return zero, &issuesync.StatusError{Message: "Todoist status write could not connect"}
		}
		return zero, &issuesync.StatusError{Message: "Todoist status write response was lost", Ambiguous: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		s.client.deferRequests(resp.Header.Get("Retry-After"), 0)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &issuesync.StatusError{Message: fmt.Sprintf("Todoist status write failed (HTTP %d)", resp.StatusCode), HTTPStatus: resp.StatusCode, Ambiguous: resp.StatusCode >= 500, Blocked: resp.StatusCode >= 300 && resp.StatusCode < 500 && resp.StatusCode != 429 && resp.StatusCode != 409}
		if resp.StatusCode == 429 {
			e.RetryAfter = s.client.cooldownRemaining()
		}
		return zero, e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return zero, &issuesync.StatusError{Message: "Todoist status write response was incomplete", Ambiguous: true}
	}
	before := &db.IssueStatusObservation{Raw: observed.RawStatus, Version: observed.Version}
	verified, err := s.ReadStatus(ctx, c, StatusTarget{ID: t.ID, Prior: before})
	if err != nil || verified.Status != desired {
		return zero, &issuesync.StatusError{Message: "Todoist status write could not be verified", Ambiguous: true}
	}
	return verified, nil
}

var _ StatusSession = (*clientSession)(nil)
