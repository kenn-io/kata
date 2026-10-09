package todoistsync

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/todoistsync/todoistapi"
)

// pageLimit is the documented maximum page size for task listings.
const pageLimit = 200

// taskFrom converts Todoist's task view. Todoist reports unknown creation and
// update times as null; Kata needs both, so each falls back to the task's
// other known timestamps, and to floor when none are known.
func taskFrom(v todoistapi.ItemSyncView, floor time.Time) Task {
	t := Task{
		ID: v.ID, ProjectID: v.ProjectID, ParentID: v.ParentID, SectionID: v.SectionID,
		Content: v.Content, Description: v.Description, AddedBy: v.AddedByUID, Assignee: v.ResponsibleUID,
		Labels: v.Labels, Priority: v.Priority, Checked: v.Checked, Deleted: v.IsDeleted,
	}
	t.Recurring, _ = v.Due["is_recurring"].(bool)
	var known []time.Time
	for _, raw := range []string{v.AddedAt, v.UpdatedAt, v.CompletedAt} {
		if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			known = append(known, at.UTC())
		}
	}
	if at, err := time.Parse(time.RFC3339Nano, v.CompletedAt); err == nil {
		t.CompletedAt = new(at.UTC())
	}
	if len(known) == 0 {
		known = []time.Time{floor}
	}
	t.AddedAt = slices.MinFunc(known, time.Time.Compare)
	t.UpdatedAt = slices.MaxFunc(known, time.Time.Compare)
	return t
}

// activeTasks lists active tasks in the project, optionally under one parent.
func (s *clientSession) activeTasks(ctx context.Context, c Config, parentID string) ([]Task, error) {
	floor := c.historyFloor()
	query := todoistapi.GetTasksAPIV1TasksGetQuery{ProjectID: &c.ProjectID, Limit: new(pageLimit)}
	if parentID != "" {
		query.ParentID = &parentID
	}
	var rows []Task
	for {
		resp, err := s.api.GetTasksAPIV1TasksGetWithResponse(ctx, &todoistapi.GetTasksAPIV1TasksGetRequestOptions{Query: &query})
		if err := responseError(err, "list Todoist tasks"); err != nil {
			return nil, err
		}
		if resp == nil || resp.JSON200 == nil {
			return nil, emptyResponse("list Todoist tasks")
		}
		for _, v := range resp.JSON200.Results {
			rows = append(rows, taskFrom(v, floor))
		}
		if resp.JSON200.NextCursor == "" {
			return rows, nil
		}
		query.Cursor = &resp.JSON200.NextCursor
	}
}

// completed lists tasks completed in [since, until), never before the history
// floor. Todoist limits each request to a three-month range.
func (s *clientSession) completed(ctx context.Context, c Config, since, until time.Time) ([]Task, error) {
	floor := c.historyFloor()
	if since.Before(floor) {
		since = floor
	}
	var rows []Task
	for since.Before(until) {
		end := since.AddDate(0, 3, 0)
		if end.After(until) {
			end = until
		}
		query := todoistapi.TasksCompletedByCompletionDateAPIV1TasksCompletedByCompletionDateGetQuery{Since: since, Until: end, ProjectID: &c.ProjectID, Limit: new(pageLimit)}
		for {
			resp, err := s.api.TasksCompletedByCompletionDateAPIV1TasksCompletedByCompletionDateGetWithResponse(ctx, &todoistapi.TasksCompletedByCompletionDateAPIV1TasksCompletedByCompletionDateGetRequestOptions{Query: &query})
			if err := responseError(err, "list completed Todoist tasks"); err != nil {
				return nil, err
			}
			if resp == nil || resp.JSON200 == nil {
				return nil, emptyResponse("list completed Todoist tasks")
			}
			for _, v := range resp.JSON200.Items {
				task := taskFrom(v, floor)
				task.Checked = true
				rows = append(rows, task)
			}
			if resp.JSON200.NextCursor == nil {
				break
			}
			query.Cursor = resp.JSON200.NextCursor
		}
		since = end
	}
	return rows, nil
}

// Tasks reads completions in [since, until), then every active task.
func (s *clientSession) Tasks(ctx context.Context, c Config, since, until time.Time) ([]Task, error) {
	history, err := s.completed(ctx, c, since, until)
	if err != nil {
		return nil, err
	}
	// Read active state after history so a task reopened during that scan
	// overrides its completion record.
	active, err := s.activeTasks(ctx, c, "")
	if err != nil {
		return nil, err
	}
	return mergeTasks(active, history), nil
}

// mergeTasks keeps each task's latest completion record and lets active state
// win, so a recurring task's past occurrences never close it.
func mergeTasks(active, history []Task) []Task {
	selected := map[string]Task{}
	for _, t := range history {
		if prev, ok := selected[t.ID]; !ok || prev.UpdatedAt.Before(t.UpdatedAt) {
			selected[t.ID] = t
		}
	}
	for _, t := range active {
		selected[t.ID] = t
	}
	rows := make([]Task, 0, len(selected))
	for _, t := range selected {
		rows = append(rows, t)
	}
	slices.SortFunc(rows, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return rows
}
