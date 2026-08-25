package api //nolint:revive // package name is the stable wire-types layout.

import (
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

// ConnectorOut is the secret-free status of one configured connector instance.
type ConnectorOut struct {
	InstanceID      string                 `json:"instance_id"`
	ConnectorID     string                 `json:"connector_id,omitempty"`
	DisplayName     string                 `json:"display_name,omitempty"`
	Protocol        string                 `json:"protocol,omitempty"`
	AccountIdentity string                 `json:"account_identity,omitempty"`
	Capabilities    []connector.Capability `json:"capabilities,omitempty"`
	Healthy         bool                   `json:"healthy"`
	HealthError     string                 `json:"health_error,omitempty"`
}

// ConnectorListResponse returns all configured connector instances.
type ConnectorListResponse struct {
	Body struct {
		Connectors []ConnectorOut `json:"connectors"`
	}
}

// ConnectorPathRequest selects one connector instance by path.
type ConnectorPathRequest struct {
	Instance string `path:"instance"`
}

// ConnectorResponse returns one configured connector instance.
type ConnectorResponse struct{ Body ConnectorOut }

// ConnectorFieldsResponse returns the live fields advertised by a connector.
type ConnectorFieldsResponse struct {
	Body struct {
		Fields []connector.FieldDescriptor `json:"fields"`
	}
}

// MapConnectorFieldRequest maps one Kata planning field to an external field.
type MapConnectorFieldRequest struct {
	Instance  string `path:"instance"`
	KataField string `path:"kata_field"`
	Body      struct {
		ExternalField string `json:"external_field"`
	}
}

// UnmapConnectorFieldRequest removes one connector field mapping.
type UnmapConnectorFieldRequest struct {
	Instance  string `path:"instance"`
	KataField string `path:"kata_field"`
}

// ConnectorFieldMappingResponse returns one connector field mapping.
type ConnectorFieldMappingResponse struct{ Body ExternalFieldMappingOut }

// ExternalFieldMappingOut describes an active connector field mapping.
type ExternalFieldMappingOut struct {
	KataField         string   `json:"kata_field"`
	ExternalFieldID   string   `json:"external_field_id"`
	ExternalFieldName string   `json:"external_field_name"`
	AcceptedKinds     []string `json:"accepted_kinds"`
	Nullable          bool     `json:"nullable"`
	Writable          bool     `json:"writable"`
	SchemaRevision    string   `json:"schema_revision"`
	Active            bool     `json:"active"`
}

// ExternalRootIssueRequest selects one issue bridge and its attributed actor.
type ExternalRootIssueRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Actor     string `query:"actor,omitempty"`
}

// BindExternalRootRequest creates a bridge for one Kata issue.
type BindExternalRootRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Body      struct {
		Connector       string `json:"connector"`
		External        string `json:"external"`
		Actor           string `json:"actor,omitempty"`
		PublishComments bool   `json:"publish_comments,omitempty"`
	}
}

// ExternalRootActionRequest applies an operator action to one issue bridge.
type ExternalRootActionRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Body      struct {
		Actor  string `json:"actor,omitempty"`
		Reason string `json:"reason,omitempty"`
	}
}

// ReconcileExternalRootByKeyRequest reconciles one connector-owned root key.
type ReconcileExternalRootByKeyRequest struct {
	Instance string `path:"instance"`
	Body     struct {
		RootKey string `json:"root_key"`
	}
}

// ResolveExternalFieldRequest resolves one planning-field conflict.
type ResolveExternalFieldRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Body      struct {
		KataField string `json:"kata_field"`
		Use       string `json:"use"`
		Actor     string `json:"actor,omitempty"`
	}
}

// ResolveExternalCommentRequest resolves one uncertain published comment.
type ResolveExternalCommentRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Body      struct {
		Action            string `json:"action"`
		ExternalCommentID string `json:"external_comment_id,omitempty"`
		Actor             string `json:"actor,omitempty"`
	}
}

// ExternalRootBridgeResponse returns one bridge snapshot.
type ExternalRootBridgeResponse struct{ Body ExternalRootBridgeOut }

// ExternalRootRunResponse returns one reconciliation result.
type ExternalRootRunResponse struct{ Body ExternalRootRunOut }

// ExternalRootBridgeOut is the secret-free status of one issue bridge.
type ExternalRootBridgeOut struct {
	ID                  int64                      `json:"id"`
	UID                 string                     `json:"uid"`
	ProjectID           int64                      `json:"project_id"`
	IssueID             int64                      `json:"issue_id"`
	ConnectorInstance   string                     `json:"connector_instance"`
	Active              bool                       `json:"active"`
	Enabled             bool                       `json:"enabled"`
	ReceiveComments     bool                       `json:"receive_comments"`
	PublishComments     bool                       `json:"publish_comments"`
	CompleteExternal    bool                       `json:"complete_external"`
	PausedReason        string                     `json:"paused_reason,omitempty"`
	LastAttemptAt       *time.Time                 `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time                 `json:"last_success_at,omitempty"`
	LastErrorAt         *time.Time                 `json:"last_error_at,omitempty"`
	LastError           string                     `json:"last_error,omitempty"`
	ConsecutiveFailures int                        `json:"consecutive_failures"`
	NextAttemptAt       *time.Time                 `json:"next_attempt_at,omitempty"`
	LastExternalState   string                     `json:"last_external_state,omitempty"`
	PendingCommentUID   string                     `json:"pending_comment_uid,omitempty"`
	FieldConflicts      []ExternalFieldConflictOut `json:"field_conflicts,omitempty"`
}

// ExternalFieldConflictOut describes unresolved canonical field candidates.
type ExternalFieldConflictOut struct {
	KataField         string                     `json:"kata_field"`
	KataCandidate     *ExternalFieldCandidateOut `json:"kata_candidate,omitempty"`
	ExternalCandidate *ExternalFieldCandidateOut `json:"external_candidate,omitempty"`
	ConflictAt        *time.Time                 `json:"conflict_at,omitempty"`
}

// ExternalFieldCandidateOut is one provider-neutral planning-field value.
type ExternalFieldCandidateOut struct {
	Kind     string `json:"kind"`
	Value    string `json:"value,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// ExternalRootRunOut summarizes committed effects from one reconciliation.
type ExternalRootRunOut struct {
	Bridge             ExternalRootBridgeOut `json:"bridge"`
	RootUpdated        bool                  `json:"root_updated"`
	CommentsCreated    int                   `json:"comments_created"`
	CommentsEdited     int                   `json:"comments_edited"`
	FieldConflicts     int                   `json:"field_conflicts"`
	CompletionRequests int                   `json:"completion_requests"`
	ReopenRequests     int                   `json:"reopen_requests"`
	Paused             bool                  `json:"paused"`
}

// ExternalFieldMappingFromDB converts a stored mapping to its wire form.
func ExternalFieldMappingFromDB(mapping db.ExternalFieldMapping) ExternalFieldMappingOut {
	return ExternalFieldMappingOut{
		KataField: mapping.KataField, ExternalFieldID: mapping.ExternalFieldID,
		ExternalFieldName: mapping.ExternalFieldName, AcceptedKinds: append([]string(nil), mapping.AcceptedKinds...),
		Nullable: mapping.Nullable, Writable: mapping.Writable, SchemaRevision: mapping.SchemaRevision, Active: mapping.Active,
	}
}
