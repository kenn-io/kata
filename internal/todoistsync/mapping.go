package todoistsync

import (
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/importlabels"
)

// todoistTitlePrefix marks imported titles when title_prefix is enabled.
const todoistTitlePrefix = "[Todoist] "

// BuildImportBatch converts tasks into one Kata import batch.
func BuildImportBatch(c Config, tasks []Task) (db.ImportBatchParams, error) {
	b := db.ImportBatchParams{Source: c.SourceKey(), Actor: "todoist-sync", ReconcileStatusForUnchanged: true, ReconcileLabelsForUnchanged: map[string][]string{}}
	for _, t := range tasks {
		title := t.Content
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		seenLabels := map[string]struct{}{}
		item := db.ImportItem{
			ExternalID: "task:" + t.ID,
			Title:      title,
			Body:       t.Description + "\n\n---\nImported from Todoist: https://app.todoist.com/app/task/" + t.ID,
			Status:     "open",
			Author:     "todoist-unknown",
			// Todoist priority runs from 1 (normal) to 4 (urgent); Kata's 1 is highest.
			Priority:  new(int64(5 - t.Priority)),
			Labels:    importlabels.AppendNormalized(nil, seenLabels, t.Labels...),
			CreatedAt: t.AddedAt.Truncate(time.Millisecond),
			UpdatedAt: t.UpdatedAt.Truncate(time.Millisecond),
		}
		if c.UseTitlePrefix() {
			item.Title = todoistTitlePrefix + title
		} else {
			item.Labels = importlabels.AppendNormalized(item.Labels, seenLabels, "todoist")
		}
		if t.AddedBy != "" {
			item.Author = "todoist:" + t.AddedBy
		}
		if t.Assignee != "" {
			item.Owner = new("todoist:" + t.Assignee)
		}
		if t.Checked {
			item.Status = "closed"
			item.ClosedReason = new("done")
			if t.CompletedAt != nil {
				item.ClosedAt = new(t.CompletedAt.Truncate(time.Millisecond))
			}
		}
		b.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"todoist"}
		b.Items = append(b.Items, item)
	}
	if err := db.ValidateImportBatch(b); err != nil {
		return db.ImportBatchParams{}, err
	}
	return b, nil
}
