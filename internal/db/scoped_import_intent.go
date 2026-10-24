package db

import (
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// ScopedImportIntent is the closed public-normalized V3 input. Actor authority,
// local IDs, provider controls and proposed target/birth UIDs are not wire data.
// The root alone runs the native import; no producer original is synthesized.
type ScopedImportIntent struct {
	ProjectUID string             `json:"project_uid"`
	Source     string             `json:"source"`
	Items      []ScopedImportItem `json:"items"`
}

type ScopedImportItem struct {
	ExternalID             string                `json:"external_id"`
	LegacyExternalIDs      []string              `json:"legacy_external_ids"`
	Title                  string                `json:"title"`
	Body                   string                `json:"body"`
	Author                 string                `json:"author"`
	Owner                  *string               `json:"owner"`
	Priority               *int64                `json:"priority"`
	Status                 string                `json:"status"`
	ClosedReason           *string               `json:"closed_reason"`
	CreatedAt              time.Time             `json:"created_at"`
	UpdatedAt              time.Time             `json:"updated_at"`
	ClosedAt               *time.Time            `json:"closed_at"`
	Labels                 []string              `json:"labels"`
	Comments               []ScopedImportComment `json:"comments"`
	Links                  []ScopedImportLink    `json:"links"`
	LinkTypesAuthoritative map[string]bool       `json:"link_types_authoritative"`
}

type ScopedImportComment struct {
	ExternalID        string    `json:"external_id"`
	LegacyExternalIDs []string  `json:"legacy_external_ids"`
	Author            string    `json:"author"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
}

type ScopedImportLink struct {
	Type             string `json:"type"`
	TargetExternalID string `json:"target_external_id"`
}

// Nil versus allocated-empty authority maps are native import semantics. This
// option is local to the new arm; V1/V2 transcripts retain their exact encoding.
func (in ScopedImportIntent) MarshalJSON() ([]byte, error) {
	type plain ScopedImportIntent
	return json.Marshal(plain(in), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
}

// nativeItems is a read-only shape view for the ONE existing import validator.
// It grants no native actor, resolves no target, and performs no import writes.
// The owned command builder/adapter clones values before mutable native use.
func (in ScopedImportIntent) nativeItems() []ImportItem {
	if in.Items == nil {
		return nil
	}
	items := make([]ImportItem, len(in.Items))
	for i, item := range in.Items {
		var comments []ImportComment
		if item.Comments != nil {
			comments = make([]ImportComment, len(item.Comments))
			for j, comment := range item.Comments {
				comments[j] = ImportComment{ExternalID: comment.ExternalID, LegacyExternalIDs: comment.LegacyExternalIDs, Author: comment.Author, Body: comment.Body, CreatedAt: comment.CreatedAt}
			}
		}
		var links []ImportLink
		if item.Links != nil {
			links = make([]ImportLink, len(item.Links))
			for j, link := range item.Links {
				links[j] = ImportLink{Type: link.Type, TargetExternalID: link.TargetExternalID}
			}
		}
		items[i] = ImportItem{ExternalID: item.ExternalID, LegacyExternalIDs: item.LegacyExternalIDs, Title: item.Title, Body: item.Body, Author: item.Author, Owner: item.Owner, Priority: item.Priority, Status: item.Status, ClosedReason: item.ClosedReason, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, ClosedAt: item.ClosedAt, Labels: item.Labels, Comments: comments, Links: links, LinkTypesAuthoritative: item.LinkTypesAuthoritative}
	}
	return items
}

func (in ScopedImportIntent) validate(actor string) error {
	if !uid.Valid(in.ProjectUID) {
		return errors.New("invalid import intent project")
	}
	// Actor is the already-declared principal's syntax input, never authority.
	// The native receiving grant and real claim/policy checks remain mandatory.
	if err := ValidateImportBatch(ImportBatchParams{Source: in.Source, Actor: actor, Items: in.nativeItems()}); err != nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if len(raw) > MaxScopedComponentOriginalBytes {
		return errors.New("scoped import intent exceeds component input bound")
	}
	return nil
}
