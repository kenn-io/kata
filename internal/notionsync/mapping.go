package notionsync

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/importlabels"
)

const maxMarkdownBytes = 1 << 20
const maxImportItemBytes = 64 << 20

// BuildImportBatch projects complete page content without supplying poll times,
// comments, upstream labels, or relationships. The caller attaches the project and guard.
func BuildImportBatch(sourceKey string, c Config, pages []PageContent) (db.ImportBatchParams, error) {
	if c.CompleteGroupID != "" {
		return db.ImportBatchParams{}, fmt.Errorf("notion group completion requires the live status schema")
	}
	return buildImportBatch(sourceKey, c, pages, nil)
}

// BuildImportBatchWithSchema classifies each page using validated live group
// membership; saved option names or order never determine completion.
func BuildImportBatchWithSchema(sourceKey string, c Config, source DataSource, pages []PageContent) (db.ImportBatchParams, error) {
	if err := ValidateSchema(c, source); err != nil {
		return db.ImportBatchParams{}, err
	}
	status, err := ResolveStatusSchema(c, source)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	return buildImportBatch(sourceKey, c, pages, &status)
}

func buildImportBatch(sourceKey string, c Config, pages []PageContent, liveStatus *StatusSchema) (db.ImportBatchParams, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	if sourceKey != "notion:"+c.DataSourceID {
		return db.ImportBatchParams{}, fmt.Errorf("notion import source does not match the configured data source")
	}
	batch := db.ImportBatchParams{Source: sourceKey, Actor: "notion-sync", ReconcileLabelsForUnchanged: map[string][]string{}, Items: make([]db.ImportItem, 0, len(pages))}
	serializedBytes := 0
	for _, content := range pages {
		page := content.Page
		pageID, err := canonicalID(page.ID)
		if err != nil {
			return db.ImportBatchParams{}, err
		}
		parentID, err := canonicalID(page.DataSourceID)
		if err != nil || parentID != c.DataSourceID {
			return db.ImportBatchParams{}, fmt.Errorf("notion page belongs to a different data source")
		}
		if page.CreatedAt.IsZero() || page.UpdatedAt.IsZero() || page.UpdatedAt.Before(page.CreatedAt) || !validSourceTime(page.CreatedAt) || !validSourceTime(page.UpdatedAt) {
			return db.ImportBatchParams{}, fmt.Errorf("notion page requires valid ordered source timestamps")
		}
		if err := validatePageURL(page.URL); err != nil {
			return db.ImportBatchParams{}, err
		}
		if !utf8.ValidString(content.Title) || strings.ContainsRune(content.Title, '\x00') {
			return db.ImportBatchParams{}, fmt.Errorf("notion title requires valid UTF-8 without NUL")
		}
		if !utf8.ValidString(content.Markdown) || len(content.Markdown) > maxMarkdownBytes {
			return db.ImportBatchParams{}, fmt.Errorf("notion markdown must be valid UTF-8 and at most 1 MiB")
		}
		title := content.Title
		if title == "" || (!c.UseTitlePrefix() && strings.TrimSpace(title) == "") {
			title = "(untitled)"
		}
		item := db.ImportItem{
			ExternalID: "page:" + pageID,
			Title:      title,
			Body:       content.Markdown + "\n---\nImported from Notion: " + page.URL,
			Author:     "notion-unknown",
			Status:     "open",
			CreatedAt:  page.CreatedAt.UTC().Truncate(time.Millisecond),
			UpdatedAt:  page.UpdatedAt.UTC().Truncate(time.Millisecond),
		}
		if c.UseTitlePrefix() {
			item.Title = "[Notion] " + title
		} else {
			item.Labels = importlabels.AppendNormalized(nil, map[string]struct{}{}, "Notion")
		}
		if page.CreatorID != "" {
			creatorID, err := canonicalID(page.CreatorID)
			if err != nil {
				return db.ImportBatchParams{}, err
			}
			item.Author = "notion:" + creatorID
		}
		if content.OwnerID != nil {
			ownerID, err := canonicalID(*content.OwnerID)
			if err != nil {
				return db.ImportBatchParams{}, err
			}
			owner := "notion:" + ownerID
			item.Owner = &owner
		}
		if liveStatus != nil {
			item.Status, err = liveStatus.Classify(page.StatusID)
			if err != nil {
				return db.ImportBatchParams{}, err
			}
		} else if page.StatusID != nil && slices.Contains(c.DoneStatusIDs, *page.StatusID) {
			item.Status = "closed"
		}
		if item.Status == "closed" {
			reason, closedAt := "done", item.UpdatedAt
			item.ClosedReason, item.ClosedAt = &reason, &closedAt
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("cannot serialize Notion import item")
		}
		serializedBytes += len(raw)
		if serializedBytes > maxImportItemBytes {
			return db.ImportBatchParams{}, fmt.Errorf("notion serialized import items exceed 64 MiB")
		}
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"notion"}
		batch.Items = append(batch.Items, item)
	}
	if err := db.ValidateImportBatch(batch); err != nil {
		return db.ImportBatchParams{}, fmt.Errorf("invalid Notion import batch")
	}
	return batch, nil
}

func validSourceTime(value time.Time) bool {
	year := value.UTC().Year()
	return year >= 1 && year <= 9999
}

func validatePageURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return fmt.Errorf("notion page URL must be a valid HTTPS URL without credentials or control characters")
	}
	return nil
}
