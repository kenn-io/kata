package db

import (
	"time"
)

// JSONBlob is the storage type for TEXT columns that hold a JSON value
// (objects for metadata, arrays for labels). Underlying string lets the
// database/sql driver scan TEXT into it directly with no `(*[]byte)(&…)`
// cast and lets handlers pass it back into INSERT/UPDATE as a regular
// string parameter. Custom MarshalJSON / UnmarshalJSON make it round-trip
// on the wire as the raw JSON value rather than as a JSON-encoded string:
// {"area":"Personal"} instead of "{\"area\":\"Personal\"}".
//
// The empty value marshals as JSON null so a struct missing a default
// doesn't produce invalid JSON. CHECK constraints in the schema
// (json_valid + json_type) enforce that any persisted value is the
// expected shape — this type intentionally trusts those constraints and
// does no shape validation on marshal.
type JSONBlob string

// MarshalJSON emits the stored bytes verbatim (a JSON object or array,
// per schema). Empty JSONBlob marshals as JSON null.
func (j JSONBlob) MarshalJSON() ([]byte, error) {
	if j == "" {
		return []byte("null"), nil
	}
	return []byte(j), nil
}

// UnmarshalJSON copies the raw input bytes verbatim. JSON null is stored
// as the empty string so a round-trip through unmarshal/marshal produces
// the same value.
func (j *JSONBlob) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*j = ""
		return nil
	}
	*j = JSONBlob(b)
	return nil
}

// Project mirrors a row in projects. DeletedAt is set when the project has
// been archived via kata projects remove (#24); the row stays in the table so
// events/issues keep referring to a valid FK target, but read paths filter it
// out. Name is the user-facing unique project key.
type Project struct {
	ID        int64      `json:"id"`
	UID       string     `json:"uid"`
	Name      string     `json:"name"`
	Metadata  JSONBlob   `json:"metadata"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// ProjectStats is the per-project aggregate returned by BatchProjectStats.
// Used by GET /api/v1/projects?include=stats. LastEventAt is nil for a
// project with zero events; tests pin this so the TUI's "—" rendering
// is exercised.
type ProjectStats struct {
	Open        int
	Closed      int
	LastEventAt *time.Time
}

// ProjectAlias mirrors a row in project_aliases.
type ProjectAlias struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"project_id"`
	AliasIdentity string    `json:"alias_identity"`
	AliasKind     string    `json:"alias_kind"`
	RootPath      string    `json:"root_path"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
}

// Issue mirrors a row in issues. Priority is 0..4 with 0 = highest priority
// and 4 = lowest; nil means no priority is set.
type Issue struct {
	ID            int64      `json:"id"`
	UID           string     `json:"uid"`
	ProjectID     int64      `json:"project_id"`
	ProjectUID    string     `json:"project_uid,omitempty"`
	ShortID       string     `json:"short_id"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Status        string     `json:"status"`
	ClosedReason  *string    `json:"closed_reason,omitempty"`
	Owner         *string    `json:"owner,omitempty"`
	Priority      *int64     `json:"priority,omitempty"`
	Author        string     `json:"author"`
	Metadata      JSONBlob   `json:"metadata"`
	Revision      int64      `json:"revision"`
	RecurrenceID  *int64     `json:"recurrence_id,omitempty"`
	OccurrenceKey *string    `json:"occurrence_key,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// ReadyGlobalIssue is an Issue paired with its project's canonical name. Used
// only by ReadyIssuesGlobal so the cross-project ready view can render
// qualified refs (`<project>#<short_id>`) without a follow-up project lookup.
type ReadyGlobalIssue struct {
	Issue
	ProjectName string `json:"project_name"`
}

// Comment mirrors a row in comments.
type Comment struct {
	ID        int64     `json:"id"`
	IssueID   int64     `json:"issue_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Event mirrors a row in events. IssueShortID and RelatedIssueShortID are
// not columns on events; they are joined from issues.short_id at read time
// so old events render correctly across short_id-shifting events (project
// merge, federation merge) — UIDs remain canonical, short_ids are display
// snapshots resolved on every read.
type Event struct {
	ID                  int64     `json:"id"`
	UID                 string    `json:"uid"`
	OriginInstanceUID   string    `json:"origin_instance_uid"`
	ProjectID           int64     `json:"project_id"`
	ProjectUID          string    `json:"project_uid"`
	ProjectName         string    `json:"project_name"`
	IssueID             *int64    `json:"issue_id,omitempty"`
	IssueUID            *string   `json:"issue_uid,omitempty"`
	IssueShortID        *string   `json:"issue_short_id,omitempty"`
	RelatedIssueID      *int64    `json:"related_issue_id,omitempty"`
	RelatedIssueUID     *string   `json:"related_issue_uid,omitempty"`
	RelatedIssueShortID *string   `json:"related_issue_short_id,omitempty"`
	Type                string    `json:"type"`
	Actor               string    `json:"actor"`
	Payload             string    `json:"payload"`
	CreatedAt           time.Time `json:"created_at"`
}

// Link mirrors a row in links.
type Link struct {
	ID           int64     `json:"id"`
	ProjectID    int64     `json:"project_id"`
	FromIssueID  int64     `json:"from_issue_id"`
	FromIssueUID string    `json:"from_issue_uid"`
	ToIssueID    int64     `json:"to_issue_id"`
	ToIssueUID   string    `json:"to_issue_uid"`
	Type         string    `json:"type"`
	Author       string    `json:"author"`
	CreatedAt    time.Time `json:"created_at"`
}

// IssueLabel mirrors a row in issue_labels.
type IssueLabel struct {
	IssueID   int64     `json:"issue_id"`
	Label     string    `json:"label"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// Recurrence mirrors a row in recurrences. Each row carries the RRULE plus
// the issue template fields (title, body, owner, priority, labels, metadata)
// used when materializing each occurrence into a concrete issues row. The
// link is closed by issues.recurrence_id + issues.occurrence_key, whose
// partial unique index guarantees one materialized issue per occurrence.
type Recurrence struct {
	ID                  int64      `json:"id"`
	UID                 string     `json:"uid"`
	ProjectID           int64      `json:"project_id"`
	RRule               string     `json:"rrule"`
	DTStart             string     `json:"dtstart"`
	Timezone            string     `json:"timezone"`
	TemplateTitle       string     `json:"template_title"`
	TemplateBody        string     `json:"template_body"`
	TemplateOwner       *string    `json:"template_owner,omitempty"`
	TemplatePriority    *int64     `json:"template_priority,omitempty"`
	TemplateLabels      JSONBlob   `json:"template_labels"`
	TemplateMetadata    JSONBlob   `json:"template_metadata"`
	NextOccurrenceKey   *string    `json:"next_occurrence_key,omitempty"`
	LastMaterializedUID *string    `json:"last_materialized_uid,omitempty"`
	Author              string     `json:"author"`
	Revision            int64      `json:"revision"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
}

// LabelCount is the per-label aggregate returned by LabelCounts.
type LabelCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// ChildCounts is the direct-child aggregate for one parent issue.
type ChildCounts struct {
	Open  int `json:"open"`
	Total int `json:"total"`
}

// SearchCandidate is one row from SearchFTS: an issue, a BM25 score (lower is
// better in raw form; we negate so higher = better), and the columns where
// the query matched. MatchedIn is the basis for the wire response's matched_in.
type SearchCandidate struct {
	Issue     Issue    `json:"issue"`
	Score     float64  `json:"score"` // BM25, negated; higher = better match
	MatchedIn []string `json:"matched_in"`
}

// IdempotencyMatch is the payload returned by LookupIdempotency. The Event row
// is included so the handler can populate `original_event` in the reuse-case
// MutationResponse without a second query.
type IdempotencyMatch struct {
	IssueID      int64
	IssueShortID string
	Fingerprint  string
	Event        Event
}

// Evidence is the typed-union element persisted on issue.closed event
// payloads (anti-agent-justification spec §3.3). It mirrors api.Evidence
// field-for-field; the daemon handler does a 1:1 conversion at the
// boundary so the db package stays free of the api dependency that
// would create an import cycle (internal/api already imports
// internal/db). Per-reason validation lives in the daemon, not here —
// the db layer treats this as opaque payload data and simply persists
// the marshaled JSON onto the event row.
type Evidence struct {
	Type string `json:"type"`

	SHA       string   `json:"sha,omitempty"`       // commit
	URL       string   `json:"url,omitempty"`       // pr
	Command   string   `json:"command,omitempty"`   // test
	Paths     []string `json:"paths,omitempty"`     // reviewed-paths
	Rationale string   `json:"rationale,omitempty"` // no-change-audit
	IssueRef  string   `json:"issue_ref,omitempty"` // duplicate-of, superseded-by
}

// PurgeLog mirrors a row in purge_log. Snapshots the issue identity at purge
// time so audits survive any future project rename. EventsDeletedMinID/MaxID
// and PurgeResetAfterEventID are nullable: NULL when no events were attached
// to the purged issue.
type PurgeLog struct {
	ID                     int64     `json:"id"`
	UID                    string    `json:"uid"`
	OriginInstanceUID      string    `json:"origin_instance_uid"`
	ProjectID              int64     `json:"project_id"`
	PurgedIssueID          int64     `json:"purged_issue_id"`
	IssueUID               *string   `json:"issue_uid,omitempty"`
	ProjectUID             *string   `json:"project_uid,omitempty"`
	ProjectName            string    `json:"project_name"`
	ShortID                *string   `json:"short_id,omitempty"`
	IssueTitle             string    `json:"issue_title"`
	IssueAuthor            string    `json:"issue_author"`
	CommentCount           int64     `json:"comment_count"`
	LinkCount              int64     `json:"link_count"`
	LabelCount             int64     `json:"label_count"`
	EventCount             int64     `json:"event_count"`
	EventsDeletedMinID     *int64    `json:"events_deleted_min_id,omitempty"`
	EventsDeletedMaxID     *int64    `json:"events_deleted_max_id,omitempty"`
	PurgeResetAfterEventID *int64    `json:"purge_reset_after_event_id,omitempty"`
	Actor                  string    `json:"actor"`
	Reason                 *string   `json:"reason,omitempty"`
	PurgedAt               time.Time `json:"purged_at"`
}
