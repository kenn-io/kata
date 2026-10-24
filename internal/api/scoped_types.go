package api

import (
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

const (
	ScopedReadMode              = "scoped-current-v1"
	ScopedReadModeHeader        = "X-Kata-Read-Mode"
	ScopedClientUpgradeRequired = "scoped_client_upgrade_required"
	ScopedHistoryUnavailable    = "scoped_history_unavailable"
)

// ScopedIssueOut deliberately has no canonical revision or activity timestamp.
// A close disposition, parent or reply is a separately restricted reference.
type ScopedIssueOut struct {
	Mode       string               `json:"mode"`
	Issue      db.ScopedIssueState  `json:"issue"`
	References []ScopedReferenceOut `json:"references"`
}

type ScopedMutationOut struct {
	Mode      string             `json:"mode"`
	Operation ScopedOperationOut `json:"operation"`
}

// Public operation output excludes private receipts, coverage and source bodies.
type ScopedOperationOut struct {
	OperationUID string `json:"operation_uid"`
	State        string `json:"state"`
	Settlement   string `json:"settlement"`
	ReasonCode   string `json:"reason_code"`
}

type ScopedControlOut struct {
	Mode   string                 `json:"mode"`
	Result db.ScopedControlResult `json:"result"`
}

type ScopedCurrentOut struct {
	Mode        string                   `json:"mode"`
	Collections ScopedCurrentCollections `json:"collections"`
	ETag        string                   `json:"etag"`
	SessionUID  string                   `json:"session_uid"`
	Position    int64                    `json:"position"`
	NextCursor  string                   `json:"next_cursor"`
	Complete    bool                     `json:"complete"`
}

// IssueReadUnion is an in-process response choice. Handlers serialize exactly
// one branch directly, preserving canonical IssueOut JSON without an envelope.
// Existing routes do not expose this until scoped negotiation is implemented.
type IssueReadUnion struct {
	Canonical *IssueOut       `json:"-"`
	Scoped    *ScopedIssueOut `json:"-"`
}

// These are caller-restricted public records, structurally separate from the
// signed transport reference/provenance records. The public query adapter explicitly adapts them
// after restriction; zero-and-marshal of a transport record is never allowed.
type ScopedReferenceOut struct {
	Slot             string               `json:"slot"`
	Kind             string               `json:"kind"`
	Subject          db.ScopedEndpoint    `json:"subject"`
	SubjectEntityUID string               `json:"subject_entity_uid"`
	Peer             *db.ScopedEndpoint   `json:"peer"`
	PeerEntityUID    string               `json:"peer_entity_uid"`
	Relation         string               `json:"relation"`
	Close            *db.ScopedCloseValue `json:"close"`
	Present          bool                 `json:"present"`
}
type ScopedProvenanceOut struct {
	ProjectUID        string `json:"project_uid"`
	Kind              string `json:"kind"`
	EntityUID         string `json:"entity_uid"`
	Creator           string `json:"creator"`
	Teammate          string `json:"teammate"`
	AttributionStatus string `json:"attribution_status"`
}
type ScopedCurrentCollections struct {
	Projects    []db.ScopedProjectState      `json:"projects"`
	Issues      []db.ScopedIssueState        `json:"issues"`
	Comments    []db.ScopedCommentState      `json:"comments"`
	Labels      []db.ScopedLabelState        `json:"labels"`
	Definitions []db.ScopedDefinitionState   `json:"definitions"`
	References  []ScopedReferenceOut         `json:"references"`
	Provenance  []ScopedProvenanceOut        `json:"provenance"`
	Artifacts   []embedding.ArtifactManifest `json:"artifacts"`
}

func (u IssueReadUnion) MarshalJSON() ([]byte, error) {
	if (u.Canonical == nil) == (u.Scoped == nil) {
		return nil, errors.New("issue read requires one response branch")
	}
	if u.Canonical != nil {
		return json.Marshal(u.Canonical)
	}
	return json.Marshal(u.Scoped)
}
