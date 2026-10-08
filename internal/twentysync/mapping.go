package twentysync

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
)

const maxItems = 10000
const maxMarkdownBytes = 1 << 20
const maxImportBytes = 64 << 20

func validSourceTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}

// BuildImportBatch prepares every task before exposing data to native storage.
func BuildImportBatch(sourceKey string, c Config, schema Schema, tasks []Task) (db.ImportBatchParams, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	if sourceKey != c.SourceKey() {
		return db.ImportBatchParams{}, fmt.Errorf("twenty source identity does not match binding")
	}
	if err := ValidateSchema(c, schema); err != nil {
		return db.ImportBatchParams{}, err
	}
	if len(tasks) > maxItems {
		return db.ImportBatchParams{}, fmt.Errorf("twenty collection exceeds 10000 tasks")
	}
	batch := db.ImportBatchParams{Source: sourceKey, Actor: "twenty-sync", ReconcileLabelsForUnchanged: map[string][]string{}, ReconcileStatusForUnchanged: true, Items: make([]db.ImportItem, 0, len(tasks))}
	size := 0
	for _, task := range tasks {
		id, err := CanonicalID(task.ID)
		if err != nil {
			return db.ImportBatchParams{}, err
		}
		if !validSourceTime(task.CreatedAt) || !validSourceTime(task.UpdatedAt) || task.UpdatedAt.Before(task.CreatedAt) {
			return db.ImportBatchParams{}, fmt.Errorf("twenty task requires ordered source timestamps")
		}
		if !utf8.ValidString(task.Title) || strings.ContainsRune(task.Title, '\x00') || !utf8.ValidString(task.Markdown) || strings.ContainsRune(task.Markdown, '\x00') || len(task.Markdown) > maxMarkdownBytes {
			return db.ImportBatchParams{}, fmt.Errorf("invalid or oversized Twenty task content")
		}
		status, reason, err := classifyStatus(c, task.Status)
		if err != nil {
			return db.ImportBatchParams{}, err
		}
		title := task.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		item := db.ImportItem{ExternalID: "task:" + id, Title: title, Body: task.Markdown + "\n---\nImported from Twenty: " + c.WebOrigin + "/object/task/" + id, Author: "twenty-unknown", Status: status, CreatedAt: task.CreatedAt.UTC().Truncate(time.Millisecond), UpdatedAt: task.UpdatedAt.UTC().Truncate(time.Millisecond)}
		if c.UseTitlePrefix() {
			item.Title = "[Twenty " + id[:8] + "] " + title
		} else {
			item.Labels = []string{"twenty"}
		}
		if task.CreatorID != "" {
			creator, err := CanonicalID(task.CreatorID)
			if err != nil {
				return db.ImportBatchParams{}, err
			}
			item.Author = "twenty:" + creator
		}
		if task.AssigneeID != nil {
			owner, err := CanonicalID(*task.AssigneeID)
			if err != nil {
				return db.ImportBatchParams{}, err
			}
			item.Owner = new("twenty:" + owner)
		}
		if status == "closed" {
			item.ClosedReason = &reason
			item.ClosedAt = new(item.UpdatedAt)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("cannot encode Twenty import item")
		}
		size += len(raw)
		if size > maxImportBytes {
			return db.ImportBatchParams{}, fmt.Errorf("twenty serialized import batch exceeds 64 MiB")
		}
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"twenty"}
		batch.Items = append(batch.Items, item)
	}
	if err := db.ValidateImportBatch(batch); err != nil {
		return db.ImportBatchParams{}, err
	}
	return batch, nil
}
