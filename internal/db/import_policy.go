package db

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// ValidateImportBatch validates backend-neutral import shape and timestamp rules.
func ValidateImportBatch(params ImportBatchParams) error {
	if params.ManageStatusSeparately && params.IssueSyncGuard == nil {
		return fmt.Errorf("%w: independent status requires a claimed provider import", ErrImportValidation)
	}
	if strings.TrimSpace(params.Source) == "" || strings.TrimSpace(params.Actor) == "" {
		return fmt.Errorf("%w: source and actor are required", ErrImportValidation)
	}
	seenItems := map[string]struct{}{}
	seenComments := map[string]struct{}{}
	for _, item := range params.Items {
		if strings.TrimSpace(item.ExternalID) == "" || strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Author) == "" {
			return fmt.Errorf("%w: external_id, title, and author are required", ErrImportValidation)
		}
		if strings.ContainsRune(item.Title, '\x00') {
			return fmt.Errorf("%w: title must not contain NUL bytes", ErrImportValidation)
		}
		if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			return fmt.Errorf("%w: created_at and updated_at are required", ErrImportValidation)
		}
		if item.UpdatedAt.Before(item.CreatedAt) {
			return fmt.Errorf("%w: updated_at cannot be before created_at", ErrImportValidation)
		}
		if _, ok := seenItems[item.ExternalID]; ok {
			return fmt.Errorf("%w: duplicate item external_id %q", ErrImportValidation, item.ExternalID)
		}
		seenItems[item.ExternalID] = struct{}{}
		if item.Status != "open" && item.Status != "closed" {
			return fmt.Errorf("%w: status must be open or closed", ErrImportValidation)
		}
		if item.ClosedAt != nil && item.ClosedAt.Before(item.CreatedAt) {
			return fmt.Errorf("%w: closed_at cannot be before created_at", ErrImportValidation)
		}
		if item.Status == "open" && (item.ClosedReason != nil || item.ClosedAt != nil) {
			return fmt.Errorf("%w: open issues cannot have closed fields", ErrImportValidation)
		}
		if item.Status == "closed" && item.ClosedAt == nil {
			return fmt.Errorf("%w: closed issues require closed_at", ErrImportValidation)
		}
		if item.ClosedReason != nil && !validImportClosedReason(*item.ClosedReason) {
			return fmt.Errorf("%w: closed_reason must be one of done, wontfix, duplicate, superseded, audit-no-change", ErrImportValidation)
		}
		if item.Priority != nil && (*item.Priority < 0 || *item.Priority > 4) {
			return fmt.Errorf("%w: priority must be between 0 and 4", ErrImportValidation)
		}
		for _, label := range item.Labels {
			if !validImportLabel(label) {
				return fmt.Errorf("%w: invalid label %q", ErrImportValidation, label)
			}
		}
		for _, comment := range item.Comments {
			if strings.TrimSpace(comment.ExternalID) == "" || strings.TrimSpace(comment.Author) == "" ||
				strings.TrimSpace(comment.Body) == "" || comment.CreatedAt.IsZero() {
				return fmt.Errorf("%w: comment external_id, author, body, and created_at are required", ErrImportValidation)
			}
			if _, ok := seenComments[comment.ExternalID]; ok {
				return fmt.Errorf("%w: duplicate comment external_id %q", ErrImportValidation, comment.ExternalID)
			}
			seenComments[comment.ExternalID] = struct{}{}
		}
		for _, link := range item.Links {
			if link.Type != "blocks" && link.Type != "parent" && link.Type != "related" {
				return fmt.Errorf("%w: link type must be parent|blocks|related", ErrImportValidation)
			}
			if strings.TrimSpace(link.TargetExternalID) == "" {
				return fmt.Errorf("%w: link target_external_id is required", ErrImportValidation)
			}
		}
	}
	return nil
}

// ImportOwnsSameSourceVersionTitle reports whether an importer may correct a
// presentation-only title without advancing the source timestamp.
func ImportOwnsSameSourceVersionTitle(mapping ImportMapping, existing Issue, item ImportItem) bool {
	if item.Title == existing.Title || mapping.SourceUpdatedAt == nil {
		return false
	}
	return SameImportTimestamp(*mapping.SourceUpdatedAt, item.UpdatedAt) &&
		SameImportTimestamp(existing.UpdatedAt, item.UpdatedAt)
}

// ImportOwnsSameSourceVersionStatus permits derived workflow changes only
// while the provider owns the exact stored scalar version. It cannot override
// a newer local edit or give an older replay current-version authority.
func ImportOwnsSameSourceVersionStatus(params ImportBatchParams, mapping ImportMapping, existing Issue, item ImportItem) bool {
	if !params.ReconcileStatusForUnchanged || mapping.SourceUpdatedAt == nil ||
		!SameImportTimestamp(*mapping.SourceUpdatedAt, item.UpdatedAt) ||
		!SameImportTimestamp(existing.UpdatedAt, item.UpdatedAt) {
		return false
	}
	return item.Status != existing.Status || !equalImportOptionalString(item.ClosedReason, existing.ClosedReason)
}

// ImportOwnsUnknownSourceVersion reports whether an explicitly current
// provider observation with no source timestamp still belongs to the stored
// source version. It cannot override a newer local edit.
func ImportOwnsUnknownSourceVersion(params ImportBatchParams, mapping ImportMapping, existing Issue, item ImportItem) bool {
	if !params.ReconcileUnknownSourceTimestamp[item.ExternalID] || mapping.SourceUpdatedAt == nil ||
		!SameImportTimestamp(*mapping.SourceUpdatedAt, existing.UpdatedAt) {
		return false
	}
	if params.ReconcileStatusForUnchanged {
		return true
	}
	_, presentation := params.ReconcileLabelsForUnchanged[item.ExternalID]
	return presentation
}

// ImportOwnsUnknownSourceVersionStatus permits a current provider status
// observation with no source timestamp while the source still owns the issue
// version. It cannot override a newer local edit.
func ImportOwnsUnknownSourceVersionStatus(params ImportBatchParams, mapping ImportMapping, existing Issue, item ImportItem) bool {
	if !params.ReconcileStatusForUnchanged || !ImportOwnsUnknownSourceVersion(params, mapping, existing, item) {
		return false
	}
	return item.Status != existing.Status || !equalImportOptionalString(item.ClosedReason, existing.ClosedReason)
}

// ImportOwnsUnknownSourceVersionTitle permits a presentation title refresh
// when that item also opted into current presentation-label reconciliation.
// Without a source version, only adding or removing the importer's declared
// presentation prefix counts as presentation; other title changes may be stale.
func ImportOwnsUnknownSourceVersionTitle(params ImportBatchParams, mapping ImportMapping, existing Issue, item ImportItem) bool {
	if params.ReconcileLabelsForUnchanged == nil || params.PresentationTitlePrefix == "" {
		return false
	}
	if _, ok := params.ReconcileLabelsForUnchanged[item.ExternalID]; !ok || !ImportOwnsUnknownSourceVersion(params, mapping, existing, item) {
		return false
	}
	prefix := params.PresentationTitlePrefix
	return existing.Title == prefix+item.Title || prefix+existing.Title == item.Title
}

// ImportedStatusOnlyItem preserves unrelated scalars when the provider's
// workflow schema changes without advancing its item version.
func ImportedStatusOnlyItem(existing Issue, item ImportItem) ImportItem {
	item.Title, item.Body, item.Owner, item.Priority = existing.Title, existing.Body, existing.Owner, existing.Priority
	item.Author, item.CreatedAt, item.UpdatedAt = existing.Author, existing.CreatedAt, existing.UpdatedAt
	return item
}

// ImportedPresentationTitlePayload builds the replayable title-only correction.
func ImportedPresentationTitlePayload(source, externalID string, existing Issue, item ImportItem) (string, error) {
	payload := map[string]any{
		"source": source, "external_id": externalID,
		"updated_at": item.UpdatedAt.UTC().Format(EventTimestampFormat),
		"title":      item.Title, "old_title": existing.Title,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal import title payload: %w", err)
	}
	return string(encoded), nil
}

// ImportedCreatedAtHealedPayload builds a replayable creation-time repair.
func ImportedCreatedAtHealedPayload(source, externalID string, existing Issue, item ImportItem) (string, error) {
	payload := map[string]any{
		"source": source, "external_id": externalID,
		"created_at": item.CreatedAt.UTC().Format(EventTimestampFormat),
		"updated_at": existing.UpdatedAt.UTC().Format(EventTimestampFormat),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal created_at heal payload: %w", err)
	}
	return string(encoded), nil
}

// ImportedIssueUpdatedPayload builds the scalar diff folded by import update events.
func ImportedIssueUpdatedPayload(source, externalID string, existing Issue, item ImportItem) (string, error) {
	payload := map[string]any{
		"source": source, "external_id": externalID,
		"updated_at": item.UpdatedAt.UTC().Format(EventTimestampFormat),
	}
	if item.Title != existing.Title {
		payload["title"], payload["old_title"] = item.Title, existing.Title
	}
	if item.Body != existing.Body {
		payload["body"] = item.Body
	}
	newOwner := NormalizeImportOwner(item.Owner)
	if !equalImportOptionalString(existing.Owner, newOwner) {
		payload["owner"], payload["old_owner"] = optionalStringValue(newOwner), optionalStringValue(existing.Owner)
	}
	if !equalImportOptionalInt64(existing.Priority, item.Priority) {
		payload["priority"], payload["old_priority"] = optionalInt64Value(item.Priority), optionalInt64Value(existing.Priority)
	}
	if item.Status != existing.Status {
		payload["status"] = item.Status
	}
	if !equalImportOptionalString(existing.ClosedReason, item.ClosedReason) {
		payload["closed_reason"] = optionalStringValue(item.ClosedReason)
	}
	if !equalImportOptionalTime(existing.ClosedAt, item.ClosedAt) {
		payload["closed_at"] = optionalFormattedTime(item.ClosedAt)
	}
	if item.CreatedAt.Before(existing.CreatedAt) {
		payload["created_at"] = item.CreatedAt.UTC().Format(EventTimestampFormat)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal import event payload: %w", err)
	}
	return string(encoded), nil
}

// NormalizeImportOwner treats an empty owner as unassigned.
func NormalizeImportOwner(owner *string) *string {
	if owner == nil || *owner == "" {
		return nil
	}
	return owner
}

// ImportLabelReconcileFilter keeps stale upstream labels out of presentation
// refreshes. A nil filter reconciles all labels for created/newer issue rows;
// otherwise only the provider's explicit presentation labels are eligible.
func ImportLabelReconcileFilter(params ImportBatchParams, item ImportItem, created, sourceNewer, sourceCurrent bool) (map[string]bool, bool) {
	if created || sourceNewer {
		return nil, true
	}
	labels := params.ReconcileLabelsForUnchanged[item.ExternalID]
	if !sourceCurrent || len(labels) == 0 {
		return nil, false
	}
	filter := make(map[string]bool, len(labels))
	for _, label := range labels {
		filter[label] = true
	}
	return filter, true
}

// ImportLabelAllowed reports whether a presentation refresh includes a label.
func ImportLabelAllowed(filter map[string]bool, label string) bool {
	return filter == nil || filter[label]
}

// ImportLabelExternalID returns the stable source identity for one managed label.
func ImportLabelExternalID(issueExternalID, label string) string {
	return issueExternalID + ":label:" + label
}

// ImportLinkExternalID returns the stable source identity for one managed link.
func ImportLinkExternalID(issueExternalID string, link ImportLink) string {
	return issueExternalID + ":" + link.Type + ":" + link.TargetExternalID
}

// ImportItemLinkTypeAuthoritative applies the per-item default-authoritative rule.
func ImportItemLinkTypeAuthoritative(item ImportItem, linkType string) bool {
	if item.LinkTypesAuthoritative == nil {
		return true
	}
	if authoritative, ok := item.LinkTypesAuthoritative[linkType]; ok {
		return authoritative
	}
	return true
}

// ImportLinkReconcileFilter selects link types eligible for this import pass.
func ImportLinkReconcileFilter(
	params ImportBatchParams,
	item ImportItem,
	created bool,
	sourceNewer bool,
) (map[string]bool, bool) {
	if created || sourceNewer {
		return nil, true
	}
	if len(params.ReconcileLinkTypesForUnchanged) == 0 {
		return nil, false
	}
	filter := make(map[string]bool, len(params.ReconcileLinkTypesForUnchanged))
	for linkType, enabled := range params.ReconcileLinkTypesForUnchanged {
		if enabled && ImportItemLinkTypeAuthoritative(item, linkType) {
			filter[linkType] = true
		}
	}
	return filter, len(filter) > 0
}

// ImportLinkTypeAllowed reports whether a filtered reconciliation includes a type.
func ImportLinkTypeAllowed(filter map[string]bool, linkType string) bool {
	return filter == nil || filter[linkType]
}

// ImportLinkMappingExternalIDAllowed safely parses a source-managed link key.
func ImportLinkMappingExternalIDAllowed(issueExternalID, linkExternalID string, filter map[string]bool) bool {
	if filter == nil {
		return true
	}
	prefix := issueExternalID + ":"
	if !strings.HasPrefix(linkExternalID, prefix) {
		return false
	}
	rest := strings.TrimPrefix(linkExternalID, prefix)
	linkType, _, ok := strings.Cut(rest, ":")
	return ok && filter[linkType]
}

// SameImportTimestamp compares timestamps at the persisted millisecond precision.
func SameImportTimestamp(left, right time.Time) bool {
	return left.UTC().Format(EventTimestampFormat) == right.UTC().Format(EventTimestampFormat)
}

func validImportClosedReason(reason string) bool {
	switch reason {
	case "done", "wontfix", "duplicate", "superseded", "audit-no-change":
		return true
	default:
		return false
	}
}

func validImportLabel(label string) bool {
	if len(label) < 1 || len(label) > 64 {
		return false
	}
	for _, character := range label {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '.' || character == '_' || character == ':' || character == '-':
		default:
			return false
		}
	}
	return true
}

func equalImportOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalImportOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalImportOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return SameImportTimestamp(*left, *right)
}

func optionalStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalInt64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalFormattedTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(EventTimestampFormat)
}
