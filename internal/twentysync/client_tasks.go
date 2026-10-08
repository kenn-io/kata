package twentysync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Tasks completes bounded pagination before returning any source content. It
// returns only tasks updated after the binding's cutoff and checks content for
// those tasks alone, so an older task cannot block newer imports.
func (s *clientSession) Tasks(ctx context.Context, c Config) ([]Task, error) {
	if err := s.validate(c, true); err != nil {
		return nil, err
	}
	cutoff, err := ParseSince(c.Since)
	if err != nil {
		return nil, err
	}
	if err := s.ensureWorkspace(ctx, c); err != nil {
		return nil, err
	}
	rows := make([]Task, 0)
	seenIDs := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	total := 0
	for page := 0; ; page++ {
		if page >= maxResponsePages {
			return nil, fmt.Errorf("twenty task collection exceeds 1000 pages")
		}
		query := url.Values{"limit": {"100"}, "depth": {"0"}, "order_by": {"id[AscNullsLast]"}}
		if cursor != "" {
			query.Set("starting_after", cursor)
		}
		raw, err := s.read(ctx, http.MethodGet, "/rest/tasks?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		total += len(raw)
		if total > maxCollectionBytes {
			return nil, fmt.Errorf("twenty task collection exceeds 128 MiB")
		}
		var envelope struct {
			Data *struct {
				Tasks *[]jsontext.Value `json:"tasks"`
			} `json:"data"`
			Page pageInfo `json:"pageInfo"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data == nil || envelope.Data.Tasks == nil {
			return nil, fmt.Errorf("incomplete Twenty task collection")
		}
		for _, wire := range *envelope.Data.Tasks {
			fields, row, err := parseTaskCore(wire)
			if err != nil {
				return nil, err
			}
			if seenIDs[row.ID] {
				return nil, fmt.Errorf("duplicate Twenty task identity")
			}
			seenIDs[row.ID] = true
			if len(seenIDs) > maxItems {
				return nil, fmt.Errorf("twenty collection exceeds 10000 tasks")
			}
			if cutoff != nil && !row.UpdatedAt.After(*cutoff) {
				continue
			}
			row, err = parseTaskContent(fields, row)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		next, more, err := nextCursor(envelope.Page, seenCursors)
		if err != nil {
			return nil, err
		}
		if !more {
			return rows, nil
		}
		cursor = next
	}
}

// parseTask reads identity, status, and timestamps, plus content when asked.
func parseTask(raw jsontext.Value, content bool) (Task, error) {
	fields, task, err := parseTaskCore(raw)
	if err != nil || !content {
		return task, err
	}
	return parseTaskContent(fields, task)
}

func parseTaskCore(raw jsontext.Value) (map[string]jsontext.Value, Task, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, Task{}, fmt.Errorf("invalid Twenty task response")
	}
	for _, name := range []string{"id", "status", "createdAt", "updatedAt", "deletedAt"} {
		if _, ok := fields[name]; !ok {
			return nil, Task{}, fmt.Errorf("incomplete Twenty task response")
		}
	}
	var wire struct {
		ID        string     `json:"id"`
		Status    *string    `json:"status"`
		CreatedAt time.Time  `json:"createdAt"`
		UpdatedAt time.Time  `json:"updatedAt"`
		DeletedAt *time.Time `json:"deletedAt"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, Task{}, fmt.Errorf("invalid Twenty task status or timestamps")
	}
	id, err := CanonicalID(wire.ID)
	if err != nil || wire.DeletedAt != nil {
		return nil, Task{}, fmt.Errorf("twenty task identity is invalid or deleted")
	}
	if !validSourceTime(wire.CreatedAt) || !validSourceTime(wire.UpdatedAt) || wire.UpdatedAt.Before(wire.CreatedAt) || (wire.Status != nil && !ValidStatusValue(*wire.Status)) {
		return nil, Task{}, fmt.Errorf("invalid Twenty task status or timestamps")
	}
	return fields, Task{ID: id, Status: wire.Status, CreatedAt: wire.CreatedAt, UpdatedAt: wire.UpdatedAt}, nil
}

func parseTaskContent(fields map[string]jsontext.Value, task Task) (Task, error) {
	for _, name := range []string{"title", "bodyV2", "assigneeId", "createdBy"} {
		if _, ok := fields[name]; !ok {
			return Task{}, fmt.Errorf("incomplete Twenty task response")
		}
	}
	var title *string
	var owner *string
	var creator *struct {
		WorkspaceMemberID *string `json:"workspaceMemberId"`
	}
	if json.Unmarshal(fields["title"], &title) != nil || json.Unmarshal(fields["assigneeId"], &owner) != nil || json.Unmarshal(fields["createdBy"], &creator) != nil {
		return Task{}, fmt.Errorf("invalid Twenty task content")
	}
	if title != nil {
		task.Title = *title
	}
	if !utf8.ValidString(task.Title) || strings.ContainsRune(task.Title, '\x00') {
		return Task{}, fmt.Errorf("invalid Twenty task title")
	}
	if owner != nil {
		id, err := CanonicalID(*owner)
		if err != nil {
			return Task{}, fmt.Errorf("invalid Twenty task assignee")
		}
		task.AssigneeID = &id
	}
	if creator != nil && creator.WorkspaceMemberID != nil {
		id, err := CanonicalID(*creator.WorkspaceMemberID)
		if err != nil {
			return Task{}, fmt.Errorf("invalid Twenty task creator")
		}
		task.CreatorID = id
	}
	markdown, err := parseTaskMarkdown(fields["bodyV2"])
	if err != nil {
		return Task{}, err
	}
	task.Markdown = markdown
	return task, nil
}

// parseTaskMarkdown treats a null body as empty and refuses rich text that
// has no Markdown rendering rather than dropping it.
func parseTaskMarkdown(raw jsontext.Value) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var body map[string]jsontext.Value
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return "", fmt.Errorf("invalid Twenty task rich text")
	}
	markdown, ok := body["markdown"]
	if !ok {
		return "", fmt.Errorf("twenty task rich text is missing Markdown")
	}
	var md, bn *string
	if json.Unmarshal(markdown, &md) != nil {
		return "", fmt.Errorf("invalid Twenty task Markdown")
	}
	if blocknote, ok := body["blocknote"]; ok && json.Unmarshal(blocknote, &bn) != nil {
		return "", fmt.Errorf("invalid Twenty task rich text")
	}
	if (md == nil || *md == "") && bn != nil && !emptyBlocknote(*bn) {
		return "", fmt.Errorf("twenty task has rich text without Markdown; cannot import without losing content")
	}
	if md == nil {
		return "", nil
	}
	if !utf8.ValidString(*md) || strings.ContainsRune(*md, '\x00') || len(*md) > maxMarkdownBytes {
		return "", fmt.Errorf("invalid or oversized Twenty task Markdown")
	}
	return *md, nil
}

// emptyBlocknote recognizes empty editor documents without converting rich text.
// Unknown block types, nested blocks, and nonempty inline content fail closed.
func emptyBlocknote(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" {
		return true
	}
	var blocks []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
		Children jsontext.Value `json:"children"`
	}
	if json.Unmarshal([]byte(value), &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		children := strings.TrimSpace(string(block.Children))
		if block.Type != "paragraph" || (children != "" && children != "null" && children != "[]") {
			return false
		}
		for _, inline := range block.Content {
			if inline.Type != "text" || inline.Text == nil || *inline.Text != "" {
				return false
			}
		}
	}
	return true
}
