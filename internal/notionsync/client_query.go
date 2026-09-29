package notionsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

const maxQueryPages = 1000
const maxUniquePages = 10000

type wireParent struct {
	Type         string `json:"type"`
	DataSourceID string `json:"data_source_id"`
	DatabaseID   string `json:"database_id"`
	PageID       string `json:"page_id"`
}
type wirePage struct {
	Object      string     `json:"object"`
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	Parent      wireParent `json:"parent"`
	CreatedTime string     `json:"created_time"`
	EditedTime  string     `json:"last_edited_time"`
	Creator     struct {
		ID string `json:"id"`
	} `json:"created_by"`
	IsArchived *bool `json:"is_archived"`
	InTrash    *bool `json:"in_trash"`
	Properties map[string]struct {
		ID     string         `json:"id"`
		Type   string         `json:"type"`
		Status jsontext.Value `json:"status"`
	} `json:"properties"`
}

// observation validates identities before discarding well-formed moved rows.
func (w wirePage) observation(cfg Config) (Page, bool, error) {
	id, err := canonicalID(w.ID)
	if err != nil || w.Object != "page" {
		return Page{}, false, fmt.Errorf("invalid Notion page identity")
	}
	if w.IsArchived == nil || w.InTrash == nil {
		return Page{}, false, fmt.Errorf("missing Notion page availability markers")
	}
	p := Page{ID: id, IsArchived: *w.IsArchived, InTrash: *w.InTrash}
	if p.IsArchived || p.InTrash {
		return p, false, nil
	}
	p.URL = w.URL
	if err := validatePageURL(p.URL); err != nil {
		return Page{}, false, err
	}
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, w.CreatedTime)
	if err != nil {
		return Page{}, false, fmt.Errorf("invalid Notion page created timestamp")
	}
	p.UpdatedAt, err = time.Parse(time.RFC3339Nano, w.EditedTime)
	if err != nil || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) || !validSourceTime(p.CreatedAt) || !validSourceTime(p.UpdatedAt) {
		return Page{}, false, fmt.Errorf("invalid Notion page edit timestamp")
	}
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	if w.Creator.ID != "" {
		p.CreatorID, err = canonicalID(w.Creator.ID)
		if err != nil {
			return Page{}, false, fmt.Errorf("invalid Notion creator identity")
		}
	}
	switch w.Parent.Type {
	case "page_id":
		if _, err := canonicalID(w.Parent.PageID); err != nil {
			return Page{}, false, fmt.Errorf("invalid Notion page parent")
		}
		return p, false, nil
	case "data_source_id":
		parent, err := canonicalID(w.Parent.DataSourceID)
		if err != nil {
			return Page{}, false, fmt.Errorf("invalid Notion page parent")
		}
		p.DataSourceID = parent
		if parent != cfg.DataSourceID {
			return p, false, nil
		}
		p.DatabaseID, err = canonicalID(w.Parent.DatabaseID)
		if err != nil {
			return Page{}, false, fmt.Errorf("invalid Notion page database parent")
		}
		if p.DatabaseID != cfg.DatabaseID {
			return p, false, nil
		}
	default:
		return Page{}, false, fmt.Errorf("invalid Notion page parent type")
	}
	found := false
	for _, property := range w.Properties {
		if property.ID != cfg.StatusPropertyID {
			continue
		}
		if found || property.Type != "status" {
			return Page{}, false, fmt.Errorf("invalid Notion selected status property")
		}
		found = true
		var status *struct {
			ID string `json:"id"`
		}
		if len(property.Status) == 0 || json.Unmarshal(property.Status, &status) != nil {
			return Page{}, false, fmt.Errorf("invalid Notion selected status value")
		}
		if status != nil {
			if strings.TrimSpace(status.ID) == "" {
				return Page{}, false, fmt.Errorf("invalid Notion status identity")
			}
			value := status.ID
			p.StatusID = &value
		}
	}
	if !found {
		return Page{}, false, fmt.Errorf("missing Notion selected status property")
	}
	return p, true, nil
}
func samePage(a, b Page) bool {
	statusEqual := a.StatusID == nil && b.StatusID == nil || a.StatusID != nil && b.StatusID != nil && *a.StatusID == *b.StatusID
	return a.ID == b.ID && a.URL == b.URL && a.DataSourceID == b.DataSourceID && a.DatabaseID == b.DatabaseID && a.CreatorID == b.CreatorID && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) && a.IsArchived == b.IsArchived && a.InTrash == b.InTrash && statusEqual
}
func nextNotionCursor(hasMore bool, next *string, seen map[string]bool) (string, error) {
	if !hasMore {
		return "", nil
	}
	if next == nil || *next == "" {
		return "", fmt.Errorf("notion pagination is missing next cursor")
	}
	if seen[*next] {
		return "", fmt.Errorf("notion pagination repeated a cursor")
	}
	seen[*next] = true
	return *next, nil
}
func (s *clientSession) Pages(ctx context.Context, cfg Config, lower *time.Time) ([]Page, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	since, err := ParseSince(cfg.Since)
	if err != nil {
		return nil, err
	}
	if since != nil && (lower == nil || since.After(*lower)) {
		lower = since
	}
	query := map[string]any{"result_type": "page", "is_archived": false, "page_size": 100, "sorts": []any{map[string]any{"timestamp": "last_edited_time", "direction": "ascending"}}}
	if lower != nil {
		query["filter"] = map[string]any{"timestamp": "last_edited_time", "last_edited_time": map[string]any{"on_or_after": lower.UTC().Format(time.RFC3339Nano)}}
	}
	pages := make([]Page, 0)
	indices := map[string]int{}
	seenIDs := map[string]bool{}
	cursors := map[string]bool{}
	issuesync.ReportProgress(ctx, "pages", 0, 0)
	for range maxQueryPages {
		var wire struct {
			Object  string     `json:"object"`
			Results []wirePage `json:"results"`
			HasMore *bool      `json:"has_more"`
			Next    *string    `json:"next_cursor"`
			Status  *struct {
				Type   string `json:"type"`
				Reason string `json:"incomplete_reason"`
			} `json:"request_status"`
		}
		if err := s.request(ctx, http.MethodPost, "/v1/data_sources/"+cfg.DataSourceID+"/query", query, &wire); err != nil {
			return nil, err
		}
		if wire.Object != "list" || wire.HasMore == nil || wire.Results == nil {
			return nil, fmt.Errorf("invalid Notion query response")
		}
		if wire.Status != nil && wire.Status.Type != "complete" {
			if wire.Status.Type == "incomplete" && wire.Status.Reason == "query_result_limit_reached" {
				return nil, fmt.Errorf("notion query incomplete: query_result_limit_reached")
			}
			return nil, fmt.Errorf("notion query has non-complete request status")
		}
		for _, row := range wire.Results {
			p, include, err := row.observation(cfg)
			if err != nil {
				return nil, err
			}
			seenIDs[p.ID] = true
			if len(seenIDs) > maxUniquePages {
				return nil, fmt.Errorf("notion query exceeds 10000 unique pages")
			}
			if !include {
				continue
			}
			if index, ok := indices[p.ID]; ok {
				old := pages[index]
				if p.UpdatedAt.Equal(old.UpdatedAt) && !samePage(p, old) {
					return nil, fmt.Errorf("notion duplicate page has conflicting metadata")
				}
				if p.UpdatedAt.After(old.UpdatedAt) {
					pages[index] = p
				}
			} else {
				indices[p.ID] = len(pages)
				pages = append(pages, p)
			}
		}
		issuesync.ReportProgress(ctx, "pages", len(pages), 0)
		cursor, err := nextNotionCursor(*wire.HasMore, wire.Next, cursors)
		if err != nil {
			return nil, err
		}
		if !*wire.HasMore {
			if since != nil {
				filtered := pages[:0]
				for _, p := range pages {
					if p.UpdatedAt.After(*since) {
						filtered = append(filtered, p)
					}
				}
				pages = filtered
			}
			return pages, nil
		}
		query["start_cursor"] = cursor
	}
	return nil, fmt.Errorf("notion query exceeds 1000 response pages")
}
