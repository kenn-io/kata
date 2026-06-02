package jsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// ImportOptions controls optional import behaviors.
type ImportOptions struct {
	// NewInstance preserves the target's meta.instance_uid (the value db.Open
	// wrote on first open) instead of overwriting it with the source's. The
	// imported events.origin_instance_uid and purge_log.origin_instance_uid
	// columns are NOT rewritten — they preserve the original origins so a
	// future federation loop-detector can tell which events came from the
	// cloned-from instance versus the new local one.
	NewInstance bool
}

// Import reads JSONL records from r and inserts them into store.
func Import(ctx context.Context, r io.Reader, store db.Storage) error {
	return ImportWithOptions(ctx, r, store, ImportOptions{})
}

// ImportWithOptions decodes the JSONL stream, normalizes every source version
// to the current shape in memory (cutover reshaping + version fills), maps
// each envelope to a backend-neutral db.ImportRecord, and replays them
// atomically via store.ImportReplay. ImportWithOptions itself holds no SQL or
// transaction state — the entire atomic insert lives in db.ImportReplay.
func ImportWithOptions(ctx context.Context, r io.Reader, store db.Storage, opts ImportOptions) error {
	envs, err := NewDecoder(r).ReadAll(ctx)
	if err != nil {
		return err
	}
	exportVersion, err := validateExportVersion(envs)
	if err != nil {
		return err
	}
	// Pre-v8 envelopes lack short_id; reshape to v8 before mapping so the
	// per-envelope path only ever sees current-version-shaped issue records.
	if exportVersion < 8 {
		if err := applyCutoverV7toV8(envs); err != nil {
			return err
		}
	}
	// Local origin for the pre-v3 identity backfill: the value db.Open wrote.
	// Replaces the old readMetaInstanceUID — jsonl no longer holds a tx.
	localInstanceUID := store.InstanceUID()

	// Walk projects ahead of the main mapping pass to build the project_id ->
	// project_uid map used by the pre-v13 event content-hash backfill. The
	// data lives entirely in the same envelope stream, so this is a pure
	// in-memory step.
	projectUIDByID, err := collectProjectUIDs(envs)
	if err != nil {
		return err
	}

	recs := make([]db.ImportRecord, 0, len(envs))
	for _, env := range envs {
		rec, err := toImportRecord(env, exportVersion, localInstanceUID, projectUIDByID)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	return store.ImportReplay(ctx, recs, db.ImportOptions{
		NewInstance:                     opts.NewInstance,
		DedupeLegacyActivePendingClaims: exportVersion < 12,
		RecomputeEventContentHash:       exportVersion < db.CurrentSchemaVersion(),
	})
}

// projectImport embeds the current-shape ProjectExport and adds the legacy
// `identity` field still consumed by the pre-v2 UID fill. Every other kind
// decodes straight into its export struct; unrecognized historical fields are
// silently ignored by the decoder.
type projectImport struct {
	db.ProjectExport
	Identity string `json:"identity,omitempty"`
	// NextIssueNumber was the legacy per-project counter; pre-v8 envelopes
	// carry it but the field is decoded-and-ignored at insert time.
	NextIssueNumber int64 `json:"next_issue_number,omitempty"`
}

// issueImport embeds the current-shape IssueExport and adds the legacy
// `number` field still consumed by the pre-v2 UID fill.
type issueImport struct {
	db.IssueExport
	Number int64 `json:"number,omitempty"`
}

// eventImport embeds EventExport and adds the legacy `project_identity` field
// that v7-and-below sources carried. It is decoded so the JSON parses, but the
// importer reads only the current-shape `project_name` field.
type eventImport struct {
	db.EventExport
	LegacyProjectName string `json:"project_identity,omitempty"`
	// IssueNumber is the legacy per-project counter snapshot, dropped at v8+.
	IssueNumber *int64 `json:"issue_number,omitempty"`
}

// purgeLogImport mirrors eventImport for purge_log.
type purgeLogImport struct {
	db.PurgeLogExport
	LegacyProjectName string `json:"project_identity,omitempty"`
	IssueNumber       int64  `json:"issue_number,omitempty"`
}

// collectProjectUIDs walks the envelope stream once and returns a project_id
// -> project_uid map. Only project envelopes contribute. Used by the pre-v13
// event content-hash backfill so the content-hash input matches what would
// have been computed against the source DB.
func collectProjectUIDs(envs []Envelope) (map[int64]string, error) {
	m := map[int64]string{}
	for _, env := range envs {
		if env.Kind != KindProject {
			continue
		}
		var p projectImport
		if err := decodeData(env, &p); err != nil {
			return nil, err
		}
		// pre-v2 sources have no UID; the per-record fillProjectUID below
		// derives one in toImportRecord, but for the pre-v13 content-hash
		// backfill we need the (id, uid) pair before the event records run.
		// Re-do the fill here so the map is complete.
		if p.UID == "" {
			t, err := parseExportTime(p.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("fill project uid for content-hash map: %w", err)
			}
			uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("project:%d:%s", p.ID, p.Identity)), t)
			if err != nil {
				return nil, fmt.Errorf("fill project uid for content-hash map: %w", err)
			}
			p.UID = uid
		}
		m[p.ID] = p.UID
	}
	return m, nil
}

func toImportRecord(env Envelope, exportVersion int, localInstanceUID string, projectUIDByID map[int64]string) (db.ImportRecord, error) {
	switch env.Kind {
	case KindMeta:
		var rec metaRecord
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		m := db.MetaKV{Key: rec.Key, Value: rec.Value}
		return db.ImportRecord{Kind: string(KindMeta), Meta: &m}, nil
	case KindProject:
		var rec projectImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeProjectTimes(&rec.ProjectExport); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillProjectUID(&rec, exportVersion); err != nil {
			return db.ImportRecord{}, err
		}
		p := rec.ProjectExport
		return db.ImportRecord{Kind: string(KindProject), Project: &p}, nil
	case KindProjectAlias:
		var rec db.AliasExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeAliasTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindProjectAlias), Alias: &rec}, nil
	case KindRecurrence:
		var rec db.RecurrenceExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeRecurrenceTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindRecurrence), Recurrence: &rec}, nil
	case KindIssue:
		var rec issueImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeIssueTimes(&rec.IssueExport); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillIssueUID(&rec, exportVersion); err != nil {
			return db.ImportRecord{}, err
		}
		i := rec.IssueExport
		return db.ImportRecord{Kind: string(KindIssue), Issue: &i}, nil
	case KindComment:
		var rec db.CommentExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeCommentTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillCommentUID(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindComment), Comment: &rec}, nil
	case KindIssueLabel:
		var rec db.IssueLabelExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeIssueLabelTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindIssueLabel), Label: &rec}, nil
	case KindLink:
		var rec db.LinkExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeLinkTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindLink), Link: &rec}, nil
	case KindImportMapping:
		var rec db.ImportMappingExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeImportMappingTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindImportMapping), ImportMapping: &rec}, nil
	case KindFederationBinding:
		var rec db.FederationBindingExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeFederationBindingTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindFederationBinding), FederationBinding: &rec}, nil
	case KindFederationSyncStatus:
		var rec db.FederationSyncStatusExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeFederationSyncStatusTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindFederationSyncStatus), FederationSyncStatus: &rec}, nil
	case KindFederationQuarantine:
		var rec db.FederationQuarantineExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeFederationQuarantineTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindFederationQuarantine), FederationQuarantine: &rec}, nil
	case KindFederationEnrollment:
		var rec db.FederationEnrollmentExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeFederationEnrollmentTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindFederationEnrollment), FederationEnrollment: &rec}, nil
	case KindIssueClaim:
		var rec db.IssueClaimExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizeIssueClaimTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindIssueClaim), IssueClaim: &rec}, nil
	case KindPendingClaimRequest:
		var rec db.PendingClaimRequestExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := normalizePendingClaimRequestTimes(&rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindPendingClaimRequest), PendingClaimRequest: &rec}, nil
	case KindEvent:
		var rec eventImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if rec.ProjectName == "" && rec.LegacyProjectName != "" {
			rec.ProjectName = rec.LegacyProjectName
		}
		// Normalize the wire timestamp BEFORE content-hash computation so a
		// Go-stringified timestamp at any export version flows through the
		// same RFC3339-millis form the hash was originally computed against.
		// At the current schema, this means a pre-normalized supplied hash
		// will mismatch the recomputed one and be rejected with a
		// content_hash error.
		if err := normalizeEventTimes(&rec.EventExport); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillEventV3Identity(&rec.EventExport, exportVersion, localInstanceUID); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillEventV11ReplayFields(&rec.EventExport, exportVersion, projectUIDByID); err != nil {
			return db.ImportRecord{}, err
		}
		if rec.ProjectUID == "" {
			rec.ProjectUID = projectUIDByID[rec.ProjectID]
		}
		e := rec.EventExport
		return db.ImportRecord{Kind: string(KindEvent), Event: &e}, nil
	case KindPurgeLog:
		var rec purgeLogImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if rec.ProjectName == "" && rec.LegacyProjectName != "" {
			rec.ProjectName = rec.LegacyProjectName
		}
		if err := normalizePurgeLogTimes(&rec.PurgeLogExport); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillPurgeLogV3Identity(&rec.PurgeLogExport, exportVersion, localInstanceUID); err != nil {
			return db.ImportRecord{}, err
		}
		pl := rec.PurgeLogExport
		return db.ImportRecord{Kind: string(KindPurgeLog), PurgeLog: &pl}, nil
	case KindSQLiteSequence:
		var rec db.SequenceExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindSQLiteSequence), Sequence: &rec}, nil
	default:
		return db.ImportRecord{}, fmt.Errorf("import %s: unsupported kind", env.Kind)
	}
}

func validateExportVersion(envs []Envelope) (int, error) {
	var rec metaRecord
	if err := decodeData(envs[0], &rec); err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(rec.Value)
	if err != nil {
		return 0, fmt.Errorf("invalid export_version %q: %w", rec.Value, err)
	}
	if version > db.CurrentSchemaVersion() {
		return 0, fmt.Errorf("unsupported export_version %d for current schema version %d", version, db.CurrentSchemaVersion())
	}
	if version < 1 {
		return 0, fmt.Errorf("invalid export_version %d", version)
	}
	return version, nil
}

func decodeData(env Envelope, dst any) error {
	if err := json.Unmarshal(env.Data, dst); err != nil {
		return fmt.Errorf("decode %s data: %w", env.Kind, err)
	}
	return nil
}

func fillProjectUID(rec *projectImport, exportVersion int) error {
	if exportVersion >= 2 || rec.UID != "" {
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill project uid: %w", err)
	}
	uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("project:%d:%s", rec.ID, rec.Identity)), t)
	if err != nil {
		return fmt.Errorf("fill project uid: %w", err)
	}
	rec.UID = uid
	return nil
}

func fillIssueUID(rec *issueImport, exportVersion int) error {
	if exportVersion >= 2 || rec.UID != "" {
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill issue uid: %w", err)
	}
	uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("issue:%d:%d", rec.ProjectID, rec.Number)), t)
	if err != nil {
		return fmt.Errorf("fill issue uid: %w", err)
	}
	rec.UID = uid
	return nil
}

func fillCommentUID(rec *db.CommentExport) error {
	if rec.UID != "" {
		if !katauid.Valid(rec.UID) {
			return fmt.Errorf("invalid comment uid %q", rec.UID)
		}
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill comment uid: %w", err)
	}
	uid, err := katauid.FromStableSeed(
		[]byte(fmt.Sprintf("comment:%d:%d:%s:%s:%s", rec.IssueID, rec.ID, rec.Author, rec.Body, rec.CreatedAt)),
		t,
	)
	if err != nil {
		return fmt.Errorf("fill comment uid: %w", err)
	}
	rec.UID = uid
	return nil
}

// fillEventV3Identity backfills events.uid + events.origin_instance_uid for
// pre-v3 sources per spec §5.3. The event UID is deterministic across reruns
// (FromStableSeed of project_id+id+created_at). The origin_instance_uid is the
// destination's local instance UID — intentionally non-deterministic across
// reruns: re-cutover from the same v2 source produces a different LOCAL and
// therefore different origins on every backfilled event. v3+ sources carry
// both fields verbatim.
func fillEventV3Identity(rec *db.EventExport, exportVersion int, localInstanceUID string) error {
	if exportVersion >= 3 {
		return nil
	}
	if rec.UID == "" {
		t, err := parseExportTime(rec.CreatedAt)
		if err != nil {
			return fmt.Errorf("fill event uid: %w", err)
		}
		uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("event:%d:%d", rec.ProjectID, rec.ID)), t)
		if err != nil {
			return fmt.Errorf("fill event uid: %w", err)
		}
		rec.UID = uid
	}
	if rec.OriginInstanceUID == "" {
		rec.OriginInstanceUID = localInstanceUID
	}
	return nil
}

// fillEventV11ReplayFields populates HLC + content_hash on pre-v13 sources,
// and validates them on current-version sources. The content-hash backfill
// uses the project_uid lookup map collected ahead of time from the project
// envelopes (the project may not yet exist in the target DB at this point).
func fillEventV11ReplayFields(rec *db.EventExport, exportVersion int, projectUIDByID map[int64]string) error {
	if exportVersion >= db.CurrentSchemaVersion() {
		if rec.HLCPhysicalMS <= 0 {
			return fmt.Errorf("event %d missing hlc_physical_ms", rec.ID)
		}
		if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
		if !validContentHash(rec.ContentHash) {
			return fmt.Errorf("event %d invalid content_hash %q", rec.ID, rec.ContentHash)
		}
		// Re-verify content_hash against the (post-normalized) record. A
		// supplied hash that was computed against a pre-normalized
		// timestamp (Go's stringified time.Time) won't match the
		// canonical RFC3339-millis form normalizeEventTimes rewrote into
		// the record, so the mismatch surfaces here as a refusal rather
		// than letting subtly-divergent rows land in the events table.
		projectUID, ok := projectUIDByID[rec.ProjectID]
		if !ok {
			return fmt.Errorf("verify event content_hash: project %d not found in import stream", rec.ProjectID)
		}
		recomputed, err := db.EventContentHash(db.EventHashInput{
			UID:               rec.UID,
			OriginInstanceUID: rec.OriginInstanceUID,
			ProjectUID:        projectUID,
			ProjectName:       rec.ProjectName,
			IssueUID:          rec.IssueUID,
			RelatedIssueUID:   rec.RelatedIssueUID,
			Type:              rec.Type,
			Actor:             rec.Actor,
			HLCPhysicalMS:     rec.HLCPhysicalMS,
			HLCCounter:        rec.HLCCounter,
			CreatedAt:         rec.CreatedAt,
			Payload:           rec.Payload,
		})
		if err != nil {
			return fmt.Errorf("verify event content_hash: %w", err)
		}
		if recomputed != rec.ContentHash {
			return fmt.Errorf("event %d content_hash mismatch (supplied %s, recomputed %s)", rec.ID, rec.ContentHash, recomputed)
		}
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill event replay fields: %w", err)
	}
	if exportVersion >= 12 {
		if rec.HLCPhysicalMS <= 0 {
			return fmt.Errorf("event %d missing hlc_physical_ms", rec.ID)
		}
		if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
	} else {
		if rec.HLCPhysicalMS <= 0 {
			rec.HLCPhysicalMS = t.UTC().UnixMilli()
			rec.HLCCounter = rec.ID
		} else if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
	}
	projectUID, ok := projectUIDByID[rec.ProjectID]
	if !ok {
		return fmt.Errorf("fill event replay fields: project %d not found in import stream", rec.ProjectID)
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               rec.UID,
		OriginInstanceUID: rec.OriginInstanceUID,
		ProjectUID:        projectUID,
		ProjectName:       rec.ProjectName,
		IssueUID:          rec.IssueUID,
		RelatedIssueUID:   rec.RelatedIssueUID,
		Type:              rec.Type,
		Actor:             rec.Actor,
		HLCPhysicalMS:     rec.HLCPhysicalMS,
		HLCCounter:        rec.HLCCounter,
		CreatedAt:         rec.CreatedAt,
		Payload:           rec.Payload,
	})
	if err != nil {
		return fmt.Errorf("fill event content hash: %w", err)
	}
	rec.ContentHash = hash
	return nil
}

func validContentHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// fillPurgeLogV3Identity backfills purge_log.uid + purge_log.origin_instance_uid
// for pre-v3 sources per spec §5.3. Mirrors fillEventV3Identity.
func fillPurgeLogV3Identity(rec *db.PurgeLogExport, exportVersion int, localInstanceUID string) error {
	if exportVersion >= 3 {
		return nil
	}
	if rec.UID == "" {
		t, err := parseExportTime(rec.PurgedAt)
		if err != nil {
			return fmt.Errorf("fill purge_log uid: %w", err)
		}
		uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("purge:%d:%d", rec.ProjectID, rec.ID)), t)
		if err != nil {
			return fmt.Errorf("fill purge_log uid: %w", err)
		}
		rec.UID = uid
	}
	if rec.OriginInstanceUID == "" {
		rec.OriginInstanceUID = localInstanceUID
	}
	return nil
}

func parseExportTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
		// Go's default time.Time stringification (e.g. "2026-05-04 00:21:07 +0000 UTC").
		// Pre-v8 JSONL exports sometimes carry timestamps in this shape; the v7→v8
		// cutover normalizes them through this parser.
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q", s)
}

// rfc3339MilliLayout is the canonical wire format for kata timestamps: RFC3339
// with millisecond precision and a literal "Z" zone.
const rfc3339MilliLayout = "2006-01-02T15:04:05.000Z"

// normalizeImportTime rewrites *p to the canonical RFC3339-millis form in
// place. Pre-v8 exports may carry timestamps in Go's default time.Time
// stringification (e.g. "2026-05-04 00:21:07 +0000 UTC"); since these strings
// also flow through to event content_hash inputs, the in-place rewrite must
// happen before any record-derived hash is computed. A nil pointer or empty
// string is left untouched, so callers can pass nullable fields directly.
func normalizeImportTime(field string, p *string) error {
	if p == nil || *p == "" {
		return nil
	}
	t, err := parseExportTime(*p)
	if err != nil {
		return fmt.Errorf("normalize %s: %w", field, err)
	}
	*p = t.UTC().Format(rfc3339MilliLayout)
	return nil
}

func normalizeProjectTimes(rec *db.ProjectExport) error {
	if err := normalizeImportTime("project.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("project.deleted_at", rec.DeletedAt)
}

func normalizeAliasTimes(rec *db.AliasExport) error {
	if err := normalizeImportTime("project_alias.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("project_alias.last_seen_at", &rec.LastSeenAt)
}

func normalizeRecurrenceTimes(rec *db.RecurrenceExport) error {
	if err := normalizeImportTime("recurrence.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("recurrence.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("recurrence.deleted_at", rec.DeletedAt)
}

func normalizeIssueTimes(rec *db.IssueExport) error {
	if err := normalizeImportTime("issue.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue.closed_at", rec.ClosedAt); err != nil {
		return err
	}
	return normalizeImportTime("issue.deleted_at", rec.DeletedAt)
}

func normalizeCommentTimes(rec *db.CommentExport) error {
	return normalizeImportTime("comment.created_at", &rec.CreatedAt)
}

func normalizeIssueLabelTimes(rec *db.IssueLabelExport) error {
	return normalizeImportTime("issue_label.created_at", &rec.CreatedAt)
}

func normalizeLinkTimes(rec *db.LinkExport) error {
	return normalizeImportTime("link.created_at", &rec.CreatedAt)
}

func normalizeImportMappingTimes(rec *db.ImportMappingExport) error {
	if err := normalizeImportTime("import_mapping.source_updated_at", rec.SourceUpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("import_mapping.imported_at", &rec.ImportedAt)
}

func normalizeFederationBindingTimes(rec *db.FederationBindingExport) error {
	if err := normalizeImportTime("federation_binding.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_binding.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_binding.last_sync_at", rec.LastSyncAt)
}

func normalizeFederationSyncStatusTimes(rec *db.FederationSyncStatusExport) error {
	if err := normalizeImportTime("federation_sync_status.last_pull_started_at", rec.LastPullStartedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_sync_status.last_pull_success_at", rec.LastPullSuccessAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_sync_status.last_push_started_at", rec.LastPushStartedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_sync_status.last_push_success_at", rec.LastPushSuccessAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_sync_status.last_error_at", rec.LastErrorAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_sync_status.last_reset_at", rec.LastResetAt)
}

func normalizeFederationQuarantineTimes(rec *db.FederationQuarantineExport) error {
	if err := normalizeImportTime("federation_quarantine.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_quarantine.skipped_at", rec.SkippedAt)
}

func normalizeFederationEnrollmentTimes(rec *db.FederationEnrollmentExport) error {
	if err := normalizeImportTime("federation_enrollment.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_enrollment.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_enrollment.revoked_at", rec.RevokedAt)
}

func normalizeIssueClaimTimes(rec *db.IssueClaimExport) error {
	if err := normalizeImportTime("issue_claim.acquired_at", &rec.AcquiredAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue_claim.expires_at", rec.ExpiresAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue_claim.released_at", rec.ReleasedAt); err != nil {
		return err
	}
	return normalizeImportTime("issue_claim.updated_at", &rec.UpdatedAt)
}

func normalizePendingClaimRequestTimes(rec *db.PendingClaimRequestExport) error {
	if err := normalizeImportTime("pending_claim_request.requested_at", &rec.RequestedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("pending_claim_request.last_attempt_at", rec.LastAttemptAt); err != nil {
		return err
	}
	if err := normalizeImportTime("pending_claim_request.rejected_at", rec.RejectedAt); err != nil {
		return err
	}
	return normalizeImportTime("pending_claim_request.resolved_at", rec.ResolvedAt)
}

func normalizeEventTimes(rec *db.EventExport) error {
	return normalizeImportTime("event.created_at", &rec.CreatedAt)
}

func normalizePurgeLogTimes(rec *db.PurgeLogExport) error {
	return normalizeImportTime("purge_log.purged_at", &rec.PurgedAt)
}
