package todoistsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"time"
)

// decodeTask retains positive recurrence and hierarchy evidence for safe writeback.
// An explicit null due proves no recurrence; a due object must include its flag.
func decodeTask(raw jsontext.Value) (Task, error) {
	var row Task
	if json.Unmarshal(raw, &row) != nil {
		return Task{}, blocked("invalid Todoist task response")
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil {
		return Task{}, blocked("invalid Todoist task response")
	}
	_, parentPresent := fields["parent_id"]
	_, sectionPresent := fields["section_id"]
	row.hierarchyKnown = parentPresent && sectionPresent && (row.ParentID == "" || ValidateID(row.ParentID) == nil) && (row.SectionID == "" || ValidateID(row.SectionID) == nil)
	if updatedAt, ok := fields["updated_at"]; ok && string(updatedAt) != "null" && !row.UpdatedAt.IsZero() {
		row.updatedAtKnown = true
	}
	if due, ok := fields["due"]; ok {
		if string(due) == "null" {
			row.recurrenceKnown = true
		} else {
			var flags struct {
				Recurring *bool `json:"is_recurring"`
			}
			row.recurrenceKnown = json.Unmarshal(due, &flags) == nil && flags.Recurring != nil
		}
	}
	fillUnknownTimes(&row)
	return row, nil
}

// fillUnknownTimes bounds Todoist's null ("unknown") creation and update times
// by the task's other known timestamps. A task with none still fails validation.
func fillUnknownTimes(row *Task) {
	var known []time.Time
	for _, at := range []time.Time{row.AddedAt, row.UpdatedAt} {
		if !at.IsZero() {
			known = append(known, at)
		}
	}
	if row.CompletedAt != nil && !row.CompletedAt.IsZero() {
		known = append(known, *row.CompletedAt)
	}
	if len(known) == 0 {
		return
	}
	if row.AddedAt.IsZero() {
		row.AddedAt = slices.MinFunc(known, time.Time.Compare)
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = slices.MaxFunc(known, time.Time.Compare)
	}
}

// readBudget bounds the pages, bytes and tasks of one bounded read.
type readBudget struct{ pages, bytes, items int }

func (s *clientSession) taskPages(ctx context.Context, c Config, path, key string, query url.Values, budget *readBudget) ([]Task, error) {
	query.Set("project_id", c.ProjectID)
	query.Set("limit", "200")
	seen := map[string]bool{}
	var rows []Task
	for {
		budget.pages++
		if budget.pages > maxResponsePages {
			return nil, blocked("Todoist collection exceeds 1000 pages")
		}
		raw, err := s.get(ctx, path+"?"+query.Encode())
		if err != nil {
			return nil, err
		}
		budget.bytes += len(raw)
		if budget.bytes > maxImportBytes {
			return nil, blocked("Todoist collection exceeds 64 MiB")
		}
		var page map[string]jsontext.Value
		if json.Unmarshal(raw, &page) != nil {
			return nil, blocked("invalid Todoist task page")
		}
		payload, ok := page[key]
		if !ok || string(payload) == "null" {
			return nil, blocked("incomplete Todoist task page")
		}
		var tasks []jsontext.Value
		if json.Unmarshal(payload, &tasks) != nil {
			return nil, blocked("invalid Todoist tasks")
		}
		budget.items += len(tasks)
		if budget.items > maxItems {
			return nil, blocked("Todoist collection exceeds 10000 task observations")
		}
		for _, rawTask := range tasks {
			task, err := decodeTask(rawTask)
			if err != nil {
				return nil, err
			}
			if key == "items" {
				task.Checked = new(true)
			}
			if err := validateTask(c, task); err != nil {
				return nil, blocked("invalid or out-of-scope Todoist task observation")
			}
			if key == "results" && *task.Checked {
				return nil, blocked("Todoist active task page contains completed task")
			}
			rows = append(rows, task)
		}
		var cursor *string
		if v, ok := page["next_cursor"]; ok {
			if json.Unmarshal(v, &cursor) != nil {
				return nil, blocked("invalid Todoist pagination cursor")
			}
		} else if key != "items" {
			// Only completed history omits next_cursor on its last page.
			return nil, blocked("invalid Todoist pagination cursor")
		}
		if cursor == nil || *cursor == "" {
			return rows, nil
		}
		if len(*cursor) > 4096 || seen[*cursor] {
			return nil, blocked("Todoist pagination cursor repeats or exceeds limit")
		}
		seen[*cursor] = true
		query.Set("cursor", *cursor)
	}
}

// completed reads completions in [since, until), never before the history floor.
func (s *clientSession) completed(ctx context.Context, c Config, since, until time.Time, budget *readBudget) ([]Task, error) {
	floor, err := ParseHistorySince(c.HistorySince)
	if err != nil {
		return nil, err
	}
	if since.Before(floor) {
		since = floor
	}
	var rows []Task
	for since.Before(until) {
		end := minTime(since.Add(30*24*time.Hour), until)
		q := url.Values{"since": {since.Format(time.RFC3339Nano)}, "until": {end.Format(time.RFC3339Nano)}}
		page, err := s.taskPages(ctx, c, "/api/v1/tasks/completed/by_completion_date", "items", q, budget)
		if err != nil {
			return nil, err
		}
		for _, task := range page {
			if task.CompletedAt.Before(since) || !task.CompletedAt.Before(end) {
				return nil, blocked("Todoist completion falls outside the requested history window")
			}
		}
		rows = append(rows, page...)
		since = end
	}
	return rows, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Tasks reads completions from since until the cutoff, then every active task.
func (s *clientSession) Tasks(ctx context.Context, c Config, since, until time.Time) ([]Task, error) {
	if err := s.validate(c); err != nil {
		return nil, err
	}
	if !validTime(until) {
		return nil, blocked("invalid Todoist history cutoff")
	}
	if _, err := s.Project(ctx, c); err != nil {
		return nil, err
	}
	budget := &readBudget{}
	history, err := s.completed(ctx, c, since, until, budget)
	if err != nil {
		return nil, err
	}
	// Read active state after history pagination so a task reopened during
	// that scan overrides its now-stale completion observation.
	active, err := s.taskPages(ctx, c, "/api/v1/tasks", "results", url.Values{}, budget)
	if err != nil {
		return nil, err
	}
	return mergeTasks(c, active, history)
}

// mergeTasks never mistakes a recurring occurrence or old completion for an active task.
func mergeTasks(c Config, active, history []Task) ([]Task, error) {
	return mergeTasksWithLimit(c, active, history, maxItems)
}

// mergeTasksWithLimit keeps request-local source limits separate from the
// session history cache, which can cover multiple bounded reads. A zero limit
// retains every validated observation from those individually bounded reads.
func mergeTasksWithLimit(c Config, active, history []Task, limit int) ([]Task, error) {
	selected := map[string]Task{}
	for _, r := range history {
		if err := validateTask(c, r); err != nil {
			return nil, err
		}
		if previous, ok := selected[r.ID]; ok {
			if previous.UpdatedAt.After(r.UpdatedAt) {
				continue
			}
			if previous.UpdatedAt.Equal(r.UpdatedAt) {
				if previous.CompletedAt.After(*r.CompletedAt) {
					continue
				}
				if previous.CompletedAt.Equal(*r.CompletedAt) {
					if !reflect.DeepEqual(previous, r) {
						return nil, blocked("conflicting Todoist observations at the same version")
					}
					continue
				}
			}
		}
		selected[r.ID] = r
	}
	seen := map[string]bool{}
	for _, r := range active {
		if err := validateTask(c, r); err != nil {
			return nil, err
		}
		if seen[r.ID] {
			return nil, blocked("duplicate Todoist active task identity")
		}
		seen[r.ID] = true
		selected[r.ID] = r
	}
	keys := make([]string, 0, len(selected))
	for id := range selected {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	rows := make([]Task, 0, len(keys))
	for _, id := range keys {
		rows = append(rows, selected[id])
	}
	if limit > 0 && len(rows) > limit {
		return nil, fmt.Errorf("todoist source exceeds %d tasks", limit)
	}
	return rows, nil
}
