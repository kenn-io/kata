package todoistsync

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/importlabels"
)

const maxItems = 10000
const maxImportBytes = 64 << 20

func validateTask(c Config, r Task) error {
	for _, id := range []string{r.ID, r.ProjectID} {
		if err := ValidateID(id); err != nil {
			return err
		}
	}
	if r.ProjectID != c.ProjectID || r.Deleted == nil || *r.Deleted || r.Checked == nil {
		return fmt.Errorf("todoist task is missing, deleted or outside the selected project")
	}
	if !validTime(r.AddedAt) || !validTime(r.UpdatedAt) || r.UpdatedAt.Before(r.AddedAt) || r.Priority < 1 || r.Priority > 4 {
		return fmt.Errorf("todoist task requires ordered timestamps and priority 1-4")
	}
	if *r.Checked && (r.CompletedAt == nil || !validTime(*r.CompletedAt) || r.CompletedAt.Before(r.AddedAt) || r.CompletedAt.After(r.UpdatedAt)) {
		return fmt.Errorf("todoist completed task requires a valid completion timestamp")
	}
	for _, text := range append([]string{r.Content, r.Description}, r.Labels...) {
		if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
			return fmt.Errorf("todoist content requires valid UTF-8 without NUL")
		}
	}
	if len(r.Description) > 1<<20 {
		return fmt.Errorf("todoist description exceeds 1 MiB")
	}
	return nil
}

// BuildImportBatch validates the complete projection before guarded import.
func BuildImportBatch(source string, c Config, project Project, tasks []Task) (db.ImportBatchParams, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	if source != c.SourceKey() || project.ID != c.ProjectID {
		return db.ImportBatchParams{}, fmt.Errorf("todoist source identity does not match binding")
	}
	if len(tasks) > maxItems {
		return db.ImportBatchParams{}, fmt.Errorf("todoist source exceeds 10000 tasks")
	}
	b := db.ImportBatchParams{Source: source, Actor: "todoist-sync", ReconcileStatusForUnchanged: true, ReconcileLabelsForUnchanged: map[string][]string{}}
	seen := map[string]bool{}
	total := 0
	for _, r := range tasks {
		if err := validateTask(c, r); err != nil {
			return db.ImportBatchParams{}, err
		}
		if seen[r.ID] {
			return db.ImportBatchParams{}, fmt.Errorf("duplicate Todoist task identity")
		}
		seen[r.ID] = true
		title := r.Content
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		link := "https://app.todoist.com/app/task/" + r.ID
		seenLabels := map[string]struct{}{}
		labels := importlabels.AppendNormalized(nil, seenLabels, r.Labels...)
		item := db.ImportItem{ExternalID: "task:" + r.ID, Title: title, Body: r.Description + "\n\n---\nImported from Todoist: " + link, Status: "open", Author: "todoist-unknown", Priority: new(5 - r.Priority), Labels: labels, CreatedAt: r.AddedAt.UTC().Truncate(time.Millisecond), UpdatedAt: r.UpdatedAt.UTC().Truncate(time.Millisecond)}
		if !r.updatedAtKnown {
			if b.ReconcileUnknownSourceTimestamp == nil {
				b.ReconcileUnknownSourceTimestamp = map[string]bool{}
			}
			b.ReconcileUnknownSourceTimestamp[item.ExternalID] = true
		}
		if c.UseTitlePrefix() {
			item.Title = "[Todoist] " + title
		} else {
			item.Labels = importlabels.AppendNormalized(item.Labels, seenLabels, "todoist")
		}
		if r.AddedBy != "" {
			if err := ValidateID(r.AddedBy); err != nil {
				return db.ImportBatchParams{}, err
			}
			item.Author = "todoist:" + r.AddedBy
		}
		if r.Assignee != "" {
			if err := ValidateID(r.Assignee); err != nil {
				return db.ImportBatchParams{}, err
			}
			item.Owner = new("todoist:" + r.Assignee)
		}
		if *r.Checked {
			item.Status = "closed"
			item.ClosedReason = new("done")
			item.ClosedAt = new(r.CompletedAt.UTC().Truncate(time.Millisecond))
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("cannot serialize Todoist task")
		}
		total += len(raw)
		if total > maxImportBytes {
			return db.ImportBatchParams{}, fmt.Errorf("todoist import exceeds 64 MiB")
		}
		b.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"todoist"}
		b.Items = append(b.Items, item)
	}
	if err := db.ValidateImportBatch(b); err != nil {
		return db.ImportBatchParams{}, err
	}
	return b, nil
}
