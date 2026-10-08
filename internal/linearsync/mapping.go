package linearsync

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
)

const maxItems = 10000
const maxDescriptionBytes = 1 << 20
const maxImportBytes = 64 << 20

func stateTypes(states []State) (map[string]string, error) {
	types := map[string]string{}
	for _, s := range states {
		id, err := CanonicalID(s.ID)
		if err != nil {
			return nil, err
		}
		if _, ok := types[id]; ok || math.IsNaN(s.Position) || math.IsInf(s.Position, 0) {
			return nil, fmt.Errorf("invalid Linear workflow identity or position")
		}
		switch s.Type {
		case "triage", "backlog", "unstarted", "started", "completed", "canceled", "duplicate":
		default:
			return nil, fmt.Errorf("unsupported Linear workflow type")
		}
		types[id] = s.Type
	}
	if len(types) == 0 || len(states) > maxItems {
		return nil, fmt.Errorf("linear team requires a bounded workflow")
	}
	return types, nil
}
func matchesScope(c Config, s Scope) bool {
	return c.WorkspaceID == s.WorkspaceID && c.TeamID == s.TeamID && c.ProjectID == s.ProjectID
}
func issueScope(c Config, i Issue) bool {
	return i.TeamID == c.TeamID && (c.ProjectID == "" || i.ProjectID == c.ProjectID)
}
func issueURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u == nil {
		return false
	}
	return u.Scheme == "https" && u.Host == "linear.app" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.Contains(u.Path, "/issue/") && !strings.ContainsAny(value, "\r\n\t\\ <>\x00")
}
func issueStatus(i Issue, typ string) (string, *string, *time.Time, error) {
	if typ != "completed" && typ != "canceled" && typ != "duplicate" {
		return "open", nil, nil, nil
	}
	reason, at := "done", i.UpdatedAt
	source := i.CompletedAt
	if typ != "completed" {
		reason = "wontfix"
		source = i.CanceledAt
	}
	if source != nil {
		if !validTime(*source) || source.After(i.UpdatedAt) {
			return "", nil, nil, fmt.Errorf("invalid Linear closure timestamp")
		}
		at = *source
	}
	at = at.UTC().Truncate(time.Millisecond)
	return "closed", &reason, &at, nil
}

// BuildImportBatch validates all observations before exposing a complete batch.
func BuildImportBatch(source string, c Config, scope Scope, states []State, issues []Issue) (db.ImportBatchParams, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	if source != c.SourceKey() || !matchesScope(c, scope) {
		return db.ImportBatchParams{}, fmt.Errorf("linear source identity does not match binding")
	}
	if len(issues) > maxItems {
		return db.ImportBatchParams{}, fmt.Errorf("linear collection exceeds 10000 issues")
	}
	types, err := stateTypes(states)
	if err != nil {
		return db.ImportBatchParams{}, err
	}
	batch := db.ImportBatchParams{Source: source, Actor: "linear-sync", ReconcileLabelsForUnchanged: map[string][]string{}, ReconcileStatusForUnchanged: true, Items: make([]db.ImportItem, 0, len(issues))}
	seen := map[string]bool{}
	size := 0
	for _, i := range issues {
		id, err := CanonicalID(i.ID)
		if err != nil {
			return db.ImportBatchParams{}, err
		}
		if seen[id] {
			return db.ImportBatchParams{}, fmt.Errorf("duplicate Linear issue identity")
		}
		seen[id] = true
		if !issueScope(c, i) {
			return db.ImportBatchParams{}, fmt.Errorf("linear issue outside selected scope")
		}
		if i.ArchivedAt != nil || i.Trashed {
			continue
		}
		typ, ok := types[i.StateID]
		if !ok {
			return db.ImportBatchParams{}, fmt.Errorf("linear issue state is absent from workflow")
		}
		if !validTime(i.CreatedAt) || !validTime(i.UpdatedAt) || i.UpdatedAt.Before(i.CreatedAt) || i.Priority < 0 || i.Priority > 4 || i.Identifier == "" || !issueURL(i.URL) {
			return db.ImportBatchParams{}, fmt.Errorf("invalid Linear issue identity, priority or timestamps")
		}
		if len(i.Description) > maxDescriptionBytes || !utf8.ValidString(i.Title) || !utf8.ValidString(i.Description) || strings.ContainsAny(i.Title+i.Description+i.Identifier, "\x00") {
			return db.ImportBatchParams{}, fmt.Errorf("invalid or oversized Linear content")
		}
		title := i.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		item := db.ImportItem{ExternalID: "issue:" + id, Title: title, Body: i.Description + "\n\n---\nImported from Linear: " + i.URL, Author: "linear-unknown", CreatedAt: i.CreatedAt.UTC().Truncate(time.Millisecond), UpdatedAt: i.UpdatedAt.UTC().Truncate(time.Millisecond)}
		if c.UseTitlePrefix() {
			item.Title = "[Linear " + i.Identifier + "] " + title
		} else {
			item.Labels = []string{"linear"}
		}
		if i.Priority != 0 {
			item.Priority = new(int64(i.Priority - 1))
		}
		for _, user := range []string{i.CreatorID, i.AssigneeID} {
			if user != "" {
				if _, err := CanonicalID(user); err != nil {
					return db.ImportBatchParams{}, err
				}
			}
		}
		if i.CreatorID != "" {
			item.Author = "linear:" + i.CreatorID
		}
		if i.AssigneeID != "" {
			item.Owner = new("linear:" + i.AssigneeID)
		}
		item.Status, item.ClosedReason, item.ClosedAt, err = issueStatus(i, typ)
		if err != nil {
			return db.ImportBatchParams{}, err
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, fmt.Errorf("cannot encode Linear import")
		}
		size += len(raw)
		if size > maxImportBytes {
			return db.ImportBatchParams{}, fmt.Errorf("linear import exceeds 64 MiB")
		}
		batch.Items = append(batch.Items, item)
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"linear"}
	}
	if err := db.ValidateImportBatch(batch); err != nil {
		return db.ImportBatchParams{}, err
	}
	return batch, nil
}
