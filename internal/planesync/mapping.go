package planesync

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
)

const maxDescriptionBytes = 1 << 20
const maxImportBytes = 64 << 20
const maxItems = 10000

// BuildImportBatch converts complete source data before any imports occur.
func BuildImportBatch(sourceKey string, c Config, project Project, states []State, items []WorkItem) (db.ImportBatchParams, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	if sourceKey != c.SourceKey() || project.ID != c.ProjectID {
		return db.ImportBatchParams{}, fmt.Errorf("plane source identity does not match binding")
	}
	if len(items) > maxItems || len(states) > maxItems {
		return db.ImportBatchParams{}, fmt.Errorf("plane source exceeds 10000 items or states")
	}
	groups, err := stateGroups(states)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	batch := db.ImportBatchParams{Source: sourceKey, Actor: "plane-sync", ReconcileLabelsForUnchanged: map[string][]string{}, ReconcileStatusForUnchanged: true, Items: make([]db.ImportItem, 0, len(items))}
	bytes := 0
	for _, row := range items {
		id, err := CanonicalID(row.ID)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: %w", row.ID, err)
		}
		projectID, err := CanonicalID(row.ProjectID)
		if err != nil || projectID != c.ProjectID {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: belongs to a different project", row.ID)
		}
		stateID, err := CanonicalID(row.StateID)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: %w", row.ID, err)
		}
		group, ok := groups[stateID]
		if !ok {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: state is absent from the project schema", row.ID)
		}
		if !validSourceTime(row.CreatedAt) || !validSourceTime(row.UpdatedAt) || row.UpdatedAt.Before(row.CreatedAt) || row.SequenceID < 1 {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: requires valid sequence and ordered source timestamps", row.ID)
		}
		if !utf8.ValidString(row.Name) || strings.ContainsRune(row.Name, '\x00') {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: title requires valid UTF-8 without NUL", row.ID)
		}
		link := c.WebOrigin + "/" + c.Workspace + "/projects/" + c.ProjectID + "/issues/" + id + "/"
		body, err := descriptionMarkdown(row.DescriptionHTML, link)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: %w", row.ID, err)
		}
		title := row.Name
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		item := db.ImportItem{ExternalID: "work-item:" + id, Title: title, Body: body + "\n---\nImported from Plane: " + link, Author: "plane-unknown", Status: "open", Priority: row.Priority, CreatedAt: row.CreatedAt.UTC().Truncate(time.Millisecond), UpdatedAt: row.UpdatedAt.UTC().Truncate(time.Millisecond)}
		if c.UseTitlePrefix() {
			item.Title = fmt.Sprintf("[Plane %s-%d] %s", project.Identifier, row.SequenceID, title)
		} else {
			item.Labels = []string{"plane"}
		}
		if row.CreatorID != "" {
			creator, err := CanonicalID(row.CreatorID)
			if err != nil {
				return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: %w", row.ID, err)
			}
			item.Author = "plane:" + creator
		}
		for i, owner := range row.AssigneeIDs {
			owner, err = CanonicalID(owner)
			if err != nil {
				return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: %w", row.ID, err)
			}
			if i == 0 {
				item.Owner = new("plane:" + owner)
			}
		}
		if group == "completed" || group == "cancelled" {
			item.Status = "closed"
			reason := "done"
			if group == "cancelled" {
				reason = "wontfix"
			}
			item.ClosedReason = &reason
			item.ClosedAt = new(item.UpdatedAt)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("plane work item %q: cannot serialize Plane import item", row.ID)
		}
		bytes += len(raw)
		if bytes > maxImportBytes {
			return db.ImportBatchParams{}, fmt.Errorf("plane serialized import items exceed 64 MiB")
		}
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"plane"}
		batch.Items = append(batch.Items, item)
	}
	if err := db.ValidateImportBatch(batch); err != nil {
		return db.ImportBatchParams{}, fmt.Errorf("invalid Plane import batch: %w", err)
	}
	return batch, nil
}

func stateGroups(states []State) (map[string]string, error) {
	groups := map[string]string{}
	for _, s := range states {
		id, err := CanonicalID(s.ID)
		if err != nil {
			return nil, err
		}
		if _, ok := groups[id]; ok {
			return nil, fmt.Errorf("duplicate Plane state identity")
		}
		switch s.Group {
		case "backlog", "unstarted", "started", "completed", "cancelled", "triage":
		default:
			return nil, fmt.Errorf("unsupported Plane state group")
		}
		groups[id] = s.Group
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("plane project has no workflow states")
	}
	return groups, nil
}

func validSourceTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
