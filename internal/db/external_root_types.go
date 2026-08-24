package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	katauid "go.kenn.io/kata/internal/uid"
)

// ExternalRootClaimStaleAfter is the default freshness window used to fence
// operator mutations from an active bridge worker.
const ExternalRootClaimStaleAfter = 5 * time.Minute

// ExternalRootClaimIsFresh reports whether a binding carries a claim that an
// operator action must not invalidate. A missing start time is conservative.
func ExternalRootClaimIsFresh(binding ExternalRootBinding, staleBefore time.Time) bool {
	if binding.ClaimToken == "" {
		return false
	}
	return binding.ClaimStartedAt == nil || !binding.ClaimStartedAt.Before(staleBefore)
}

// ExternalRootBinding is the durable association between one Kata issue and
// one root resolved by a configured connector instance. It contains identity
// and delivery state only; connector configuration and credentials never
// belong in this record.
type ExternalRootBinding struct {
	ID                      int64
	ProjectID               int64
	IssueID                 int64
	RootMappingID           int64
	UID                     string
	ConnectorInstance       string
	ExternalRootKey         string
	ExternalAccountKey      string
	Active                  bool
	Enabled                 bool
	ReceiveComments         bool
	PublishComments         bool
	CompleteExternal        bool
	PausedReason            string
	LastExternalState       string
	LastExternalRevision    string
	ReceiveCommentsAfter    *time.Time
	PublishCommentsAfter    *time.Time
	ClaimToken              string
	ClaimStartedAt          *time.Time
	PendingCommentUID       string
	PendingCommentStartedAt *time.Time
	LastAttemptAt           *time.Time
	LastSuccessAt           *time.Time
	LastErrorAt             *time.Time
	LastError               string
	ConsecutiveFailures     int
	NextAttemptAt           *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
	UnboundAt               *time.Time
}

// ExternalRootCommentMappingSource scopes inbound provider comment identities
// to one durable root binding while leaving the connector's root mapping
// namespace unchanged.
func ExternalRootCommentMappingSource(binding ExternalRootBinding) string {
	return "connector:" + binding.ConnectorInstance + ":binding:" + binding.UID
}

// ExternalRootLifecycleMappingSource keeps synthetic lifecycle comments out of
// the provider-controlled comment identity namespace.
func ExternalRootLifecycleMappingSource(binding ExternalRootBinding) string {
	return ExternalRootCommentMappingSource(binding) + ":lifecycle"
}

// ExternalRootPublishedCommentMappingSource scopes comments published from
// Kata separately from inbound projections. This direction marker prevents a
// later provider edit from rewriting a locally authored comment in place.
func ExternalRootPublishedCommentMappingSource(binding ExternalRootBinding) string {
	return ExternalRootCommentMappingSource(binding) + ":published"
}

// ExternalRootCommentRevisionMappingSource scopes durable provider comment
// revision observations to one binding without adding provider-specific state.
func ExternalRootCommentRevisionMappingSource(binding ExternalRootBinding) string {
	return ExternalRootCommentMappingSource(binding) + ":revisions"
}

// ExternalRootRevisionMappingSource scopes durable root revisions to one
// binding while preserving LastExternalRevision as the last completed run.
func ExternalRootRevisionMappingSource(binding ExternalRootBinding) string {
	return ExternalRootCommentMappingSource(binding) + ":root-revisions"
}

// ExternalRootSkippedCommentMappingSource scopes durable skip markers for
// local comments that must never publish outbound: comments that existed
// when a publishing binding was created.
func ExternalRootSkippedCommentMappingSource(connectorInstance string) string {
	return "connector-skip:" + connectorInstance
}

const (
	// ExternalRevisionAnchorObjectType anchors binding-specific revision records
	// to their owning issue without requiring a synthetic local comment.
	ExternalRevisionAnchorObjectType = "issue"
	// ExternalCommentFrontierExternalID is the singleton identity-frontier marker.
	ExternalCommentFrontierExternalID = "enabled"
)

// ExternalCommentRevisionMappingExternalID returns an opaque identity for one
// provider comment revision. The provider's raw identity remains out of the
// mapping namespace while equality stays deterministic across restarts.
func ExternalCommentRevisionMappingExternalID(externalID, revision string) string {
	digest := sha256.Sum256([]byte(externalID + "\x00" + revision))
	return hex.EncodeToString(digest[:])
}

// ExternalRootRevisionMappingExternalID returns an opaque identity for one
// stable provider root revision.
func ExternalRootRevisionMappingExternalID(externalRootKey, revision string) string {
	digest := sha256.Sum256([]byte(externalRootKey + "\x00" + revision))
	return hex.EncodeToString(digest[:])
}

// ValidateExternalRootBindingReplayIdentity checks that a replayed binding's
// durable root mapping names the same connector root as the binding itself.
func ValidateExternalRootBindingReplayIdentity(binding ExternalRootBindingExport) error {
	if !katauid.Valid(binding.UID) {
		return fmt.Errorf("%w: valid binding UID is required", ErrExternalRootValidation)
	}
	if binding.CreatedAt.IsZero() || binding.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: binding timestamps are required", ErrExternalRootValidation)
	}
	wantSource := "connector:" + binding.ConnectorInstance
	if binding.RootMappingSource != wantSource || binding.RootMappingExternalID != binding.ExternalRootKey {
		return fmt.Errorf("%w: root mapping identity does not match binding", ErrExternalRootValidation)
	}
	if (binding.PendingCommentUID == "") != (binding.PendingCommentStartedAt == nil) {
		return fmt.Errorf("%w: pending comment UID and start time must be supplied together", ErrExternalRootValidation)
	}
	return nil
}

// ExternalFieldMapping is one version of a connector-instance planning-field
// mapping. Reconfiguration deactivates the prior row and inserts a new row so
// field-state history remains attached to the descriptor it used.
type ExternalFieldMapping struct {
	ID                int64
	ConnectorInstance string
	KataField         string
	ExternalFieldID   string
	ExternalFieldName string
	SchemaRevision    string
	AcceptedKinds     []string
	Nullable          bool
	Writable          bool
	Active            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ExternalFieldState stores the three-way merge baseline and an optional
// conflict for one binding and one historical mapping descriptor.
type ExternalFieldState struct {
	BindingID        int64
	MappingID        int64
	Baseline         json.RawMessage
	ConflictKata     json.RawMessage
	ConflictExternal json.RawMessage
	Conflicted       bool
	ConflictAt       *time.Time
	UpdatedAt        time.Time
}

// CreateExternalRootBindingParams defines one atomic issue-to-root binding.
type CreateExternalRootBindingParams struct {
	ProjectID            int64
	IssueID              int64
	ConnectorInstance    string
	ExternalRootKey      string
	ExternalAccountKey   string
	Actor                string
	ReceiveCommentsAfter time.Time
	PublishComments      bool
	PublishCommentsAfter *time.Time
	// UseLocalPublishFrontier asks storage to suppress existing local comments
	// and set a lower-bound PublishCommentsAfter in the binding transaction.
	// This serializes the enable boundary with local comment creation.
	UseLocalPublishFrontier bool
	// UseCommentIdentityFrontier records the comment identities returned by the
	// bind-time list operation atomically with the binding. An empty list is a
	// meaningful captured frontier.
	UseCommentIdentityFrontier bool
	InitialCommentRevisions    []ExternalCommentRevision
	// InitialClaimToken and InitialClaimStartedAt reserve the first reconcile
	// attempt in the same transaction that creates the binding. They must be
	// supplied together; ordinary creation leaves both at their zero values.
	InitialClaimToken     string
	InitialClaimStartedAt time.Time
}

// ExternalCommentRevision names one stable provider comment observation.
type ExternalCommentRevision struct {
	ExternalID string
	Revision   string
}

// ExternalRootSuccessParams finalizes one claimed reconciliation successfully.
type ExternalRootSuccessParams struct {
	BindingID        int64
	ClaimToken       string
	At               time.Time
	NextAttemptAt    time.Time
	ExternalState    string
	ExternalRevision string
}

// ExternalRootErrorParams finalizes one claimed reconciliation with an error.
type ExternalRootErrorParams struct {
	BindingID        int64
	ClaimToken       string
	At               time.Time
	NextAttemptAt    time.Time
	Error            string
	ExternalState    string
	ExternalRevision string
}

// ValidateExternalRootSuccessParams validates a successful reconciliation checkpoint.
func ValidateExternalRootSuccessParams(params ExternalRootSuccessParams) error {
	if err := validateExternalRootCheckpointBase(
		params.BindingID, params.ClaimToken, params.At, params.NextAttemptAt,
	); err != nil {
		return err
	}
	if !validExternalRootState(params.ExternalState) || strings.TrimSpace(params.ExternalRevision) == "" {
		return fmt.Errorf("%w: external state and revision are required", ErrExternalRootValidation)
	}
	return nil
}

// ValidateExternalRootErrorParams validates a failed reconciliation checkpoint.
func ValidateExternalRootErrorParams(params ExternalRootErrorParams) error {
	if err := validateExternalRootCheckpointBase(
		params.BindingID, params.ClaimToken, params.At, params.NextAttemptAt,
	); err != nil {
		return err
	}
	hasExternalState := strings.TrimSpace(params.ExternalState) != ""
	hasExternalRevision := strings.TrimSpace(params.ExternalRevision) != ""
	if hasExternalState != hasExternalRevision {
		return fmt.Errorf("%w: external state and revision must be supplied together", ErrExternalRootValidation)
	}
	if hasExternalState && !validExternalRootState(params.ExternalState) {
		return fmt.Errorf("%w: external state must be open or complete", ErrExternalRootValidation)
	}
	return nil
}

func validateExternalRootCheckpointBase(
	bindingID int64,
	claimToken string,
	at time.Time,
	nextAttemptAt time.Time,
) error {
	if bindingID <= 0 || strings.TrimSpace(claimToken) == "" || at.IsZero() || nextAttemptAt.IsZero() {
		return fmt.Errorf("%w: binding, claim, and attempt times are required", ErrExternalRootValidation)
	}
	return nil
}

func validExternalRootState(state string) bool {
	return state == "open" || state == "complete"
}

// ExternalRootActionParams applies one audited bridge lifecycle action.
type ExternalRootActionParams struct {
	BindingID int64
	// ClaimToken is optional for explicit operator actions. Connector-driven
	// pauses supply it so storage proves the active, enabled claim in the same
	// transaction that disables the binding.
	ClaimToken string
	// StaleBefore lets explicit operator actions recover an abandoned claim.
	// A nonempty claim at or after this instant fences pause and unbind.
	StaleBefore time.Time
	Actor       string
	Reason      string
}

// ExternalRootProjectionParams carries one claim-guarded native root projection.
type ExternalRootProjectionParams struct {
	BindingID          int64
	ClaimToken         string
	Title              string
	Body               string
	ExternalRevision   string
	ExternalActorID    string
	ExternalActorName  string
	ExternalUpdatedAt  time.Time
	ExternalObservedAt time.Time
	IntegrationActor   string
}

// ExternalCommentProjectionParams carries one claim-guarded comment projection.
type ExternalCommentProjectionParams struct {
	BindingID         int64
	ClaimToken        string
	ExternalID        string
	ExternalRevision  string
	LifecycleState    string
	Body              string
	ExternalActorID   string
	ExternalActorName string
	ExternalCreatedAt time.Time
	ExternalUpdatedAt time.Time
	Deleted           bool
	IntegrationActor  string
}

// SetPendingExternalCommentParams records an outbound comment before publication.
type SetPendingExternalCommentParams struct {
	BindingID  int64
	ClaimToken string
	CommentUID string
	At         time.Time
}

// ClearPendingExternalCommentParams resolves a pending outbound comment.
type ClearPendingExternalCommentParams struct {
	BindingID    int64
	ClaimToken   string
	CommentUID   string
	ExpectedBody string
	Action       string
	Actor        string
	At           time.Time
	Mapping      *ImportMappingParams
	// ExternalRevision is required when Action publishes or adopts Mapping.
	ExternalRevision string
}

// ExternalFieldMappingParams defines one validated connector field mapping.
type ExternalFieldMappingParams struct {
	ConnectorInstance string
	KataField         string
	ExternalFieldID   string
	ExternalFieldName string
	AcceptedKinds     []string
	Nullable          bool
	Writable          bool
	SchemaRevision    string
}

// ExternalFieldStateParams advances one claim-guarded field baseline or conflict.
type ExternalFieldStateParams struct {
	BindingID        int64
	MappingID        int64
	ClaimToken       string
	Baseline         json.RawMessage
	ConflictKata     json.RawMessage
	ConflictExternal json.RawMessage
	Conflicted       bool
	At               time.Time
	Actor            string
}

// ExternalFieldProjectionParams carries the closed native metadata mutation
// for one mapped planning field. Patch may contain only KataField and the
// shared timezone key; connector settings and arbitrary external diagnostics
// cannot cross this boundary.
type ExternalFieldProjectionParams struct {
	BindingID             int64
	MappingID             int64
	ClaimToken            string
	KataField             string
	Patch                 map[string]json.RawMessage
	ExpectedIssueRevision int64
	IntegrationActor      string
}

// ResolveExternalFieldConflictParams resolves one stored field conflict.
type ResolveExternalFieldConflictParams struct {
	BindingID  int64
	MappingID  int64
	ClaimToken string
	Baseline   json.RawMessage
	Actor      string
	At         time.Time
}

// ExternalProjectionSource is the closed external provenance envelope carried
// by native issue and comment mutation events. Connector settings, credentials,
// and diagnostics cannot be attached to this shape.
type ExternalProjectionSource struct {
	ConnectorInstance string `json:"connector_instance"`
	ExternalRootKey   string `json:"external_root_key,omitempty"`
	ExternalCommentID string `json:"external_comment_id,omitempty"`
	ExternalRevision  string `json:"external_revision"`
	ActorID           string `json:"actor_id,omitempty"`
	ActorName         string `json:"actor_name,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	ObservedAt        string `json:"observed_at,omitempty"`
	Deleted           bool   `json:"deleted,omitempty"`
}

type externalRootProjectionPayload struct {
	Title     *string                  `json:"title,omitempty"`
	OldTitle  *string                  `json:"old_title,omitempty"`
	Body      *string                  `json:"body,omitempty"`
	UpdatedAt string                   `json:"updated_at"`
	Source    ExternalProjectionSource `json:"source"`
}

type externalCommentProjectionPayload struct {
	CommentUID string                   `json:"comment_uid"`
	Author     string                   `json:"author,omitempty"`
	Body       string                   `json:"body"`
	CreatedAt  string                   `json:"created_at,omitempty"`
	EditedAt   string                   `json:"edited_at,omitempty"`
	Source     ExternalProjectionSource `json:"source"`
}

// ValidateExternalRootProjectionParams validates a native root projection.
func ValidateExternalRootProjectionParams(params ExternalRootProjectionParams) error {
	if params.BindingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.ExternalRevision) == "" || strings.TrimSpace(params.IntegrationActor) == "" {
		return fmt.Errorf("%w: binding, claim, revision, and integration actor are required", ErrExternalRootValidation)
	}
	if containsNUL(params.ClaimToken, params.Title, params.Body, params.ExternalRevision,
		params.ExternalActorID, params.ExternalActorName, params.IntegrationActor) {
		return fmt.Errorf("%w: external root projection contains NUL", ErrExternalRootValidation)
	}
	if params.ExternalUpdatedAt.IsZero() || params.ExternalObservedAt.IsZero() {
		return fmt.Errorf("%w: external update and observation timestamps are required", ErrExternalRootValidation)
	}
	if params.ExternalObservedAt.Before(params.ExternalUpdatedAt) {
		return fmt.Errorf("%w: external observation must not precede the external update", ErrExternalRootValidation)
	}
	if strings.TrimSpace(params.Title) == "" || strings.ContainsRune(params.Title, '\x00') {
		return fmt.Errorf("%w: external root title is invalid", ErrExternalRootValidation)
	}
	return nil
}

// ValidateExternalCommentProjectionParams validates a native comment projection.
func ValidateExternalCommentProjectionParams(params ExternalCommentProjectionParams) error {
	if params.BindingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.ExternalID) == "" || strings.TrimSpace(params.ExternalRevision) == "" ||
		strings.TrimSpace(params.IntegrationActor) == "" {
		return fmt.Errorf("%w: binding, claim, external comment, revision, and integration actor are required", ErrExternalRootValidation)
	}
	if containsNUL(params.ClaimToken, params.ExternalID, params.ExternalRevision, params.LifecycleState,
		params.Body, params.ExternalActorID, params.ExternalActorName, params.IntegrationActor) {
		return fmt.Errorf("%w: external comment projection contains NUL", ErrExternalRootValidation)
	}
	if params.ExternalCreatedAt.IsZero() || params.ExternalUpdatedAt.IsZero() {
		return fmt.Errorf("%w: external comment timestamps are required", ErrExternalRootValidation)
	}
	if params.ExternalUpdatedAt.Before(params.ExternalCreatedAt) {
		return fmt.Errorf("%w: external comment update must not precede its creation", ErrExternalRootValidation)
	}
	return nil
}

// ValidateExternalRootLifecycleRequestParams validates an atomic lifecycle request.
func ValidateExternalRootLifecycleRequestParams(params ExternalCommentProjectionParams) error {
	if err := ValidateExternalCommentProjectionParams(params); err != nil {
		return err
	}
	if params.Deleted || !strings.HasPrefix(params.ExternalID, "lifecycle:") ||
		(params.LifecycleState != "open" && params.LifecycleState != "complete") {
		return fmt.Errorf("%w: lifecycle request must use a lifecycle external id", ErrExternalRootValidation)
	}
	return nil
}

// MarshalExternalRootProjectionPayload builds the closed root event payload.
func MarshalExternalRootProjectionPayload(
	binding ExternalRootBinding,
	current Issue,
	params ExternalRootProjectionParams,
	updatedAt string,
) (string, error) {
	payload := externalRootProjectionPayload{
		UpdatedAt: updatedAt,
		Source: ExternalProjectionSource{
			ConnectorInstance: binding.ConnectorInstance,
			ExternalRootKey:   binding.ExternalRootKey,
			ExternalRevision:  params.ExternalRevision,
			ActorID:           params.ExternalActorID,
			ActorName:         params.ExternalActorName,
			UpdatedAt:         params.ExternalUpdatedAt.UTC().Format(time.RFC3339Nano),
			ObservedAt:        params.ExternalObservedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if current.Title != params.Title {
		payload.Title = &params.Title
		payload.OldTitle = &current.Title
	}
	if current.Body != params.Body {
		payload.Body = &params.Body
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal external root projection payload: %w", err)
	}
	return string(body), nil
}

// MarshalExternalCommentProjectionPayload builds the closed comment event payload.
func MarshalExternalCommentProjectionPayload(
	binding ExternalRootBinding,
	comment Comment,
	params ExternalCommentProjectionParams,
	mutationAt string,
	created bool,
) (string, error) {
	payload := externalCommentProjectionPayload{
		CommentUID: comment.UID,
		Body:       comment.Body,
		Source: ExternalProjectionSource{
			ConnectorInstance: binding.ConnectorInstance,
			ExternalRootKey:   binding.ExternalRootKey,
			ExternalCommentID: params.ExternalID,
			ExternalRevision:  params.ExternalRevision,
			ActorID:           params.ExternalActorID,
			ActorName:         params.ExternalActorName,
			CreatedAt:         params.ExternalCreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt:         params.ExternalUpdatedAt.UTC().Format(time.RFC3339Nano),
			Deleted:           params.Deleted,
		},
	}
	if created {
		payload.Author = comment.Author
		payload.CreatedAt = params.ExternalCreatedAt.UTC().Format(time.RFC3339Nano)
	} else {
		payload.EditedAt = mutationAt
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal external comment projection payload: %w", err)
	}
	return string(body), nil
}

// ExternalRootAuditPayload is deliberately closed: callers cannot attach
// connector settings, credentials, or arbitrary diagnostic text to an event.
type ExternalRootAuditPayload struct {
	BindingUID        string `json:"binding_uid"`
	ConnectorInstance string `json:"connector_instance"`
	ExternalRootKey   string `json:"external_root_key"`
	Action            string `json:"action"`
	Actor             string `json:"actor"`
	Active            bool   `json:"active"`
	Enabled           bool   `json:"enabled"`
	PendingCommentUID string `json:"pending_comment_uid,omitempty"`
	KataField         string `json:"kata_field,omitempty"`
}

// MarshalExternalRootAuditPayload builds the closed bridge audit event payload.
func MarshalExternalRootAuditPayload(
	binding ExternalRootBinding,
	action string,
	actor string,
	pendingCommentUID string,
	kataField string,
) (string, error) {
	payload, err := json.Marshal(ExternalRootAuditPayload{
		BindingUID: binding.UID, ConnectorInstance: binding.ConnectorInstance,
		ExternalRootKey: binding.ExternalRootKey, Action: action, Actor: actor,
		Active: binding.Active, Enabled: binding.Enabled,
		PendingCommentUID: pendingCommentUID, KataField: kataField,
	})
	if err != nil {
		return "", fmt.Errorf("marshal external root audit payload: %w", err)
	}
	return string(payload), nil
}

// ValidateCreateExternalRootBindingParams validates binding identity and frontiers.
func ValidateCreateExternalRootBindingParams(params CreateExternalRootBindingParams) error {
	if params.ProjectID <= 0 || params.IssueID <= 0 {
		return fmt.Errorf("%w: project and issue are required", ErrExternalRootValidation)
	}
	if strings.TrimSpace(params.ConnectorInstance) == "" ||
		strings.TrimSpace(params.ExternalRootKey) == "" ||
		strings.TrimSpace(params.ExternalAccountKey) == "" {
		return fmt.Errorf("%w: connector, root, and account identities are required", ErrExternalRootValidation)
	}
	if strings.TrimSpace(params.Actor) == "" {
		return fmt.Errorf("%w: actor is required", ErrExternalRootValidation)
	}
	if containsNUL(params.ConnectorInstance, params.ExternalRootKey, params.ExternalAccountKey,
		params.Actor, params.InitialClaimToken) {
		return fmt.Errorf("%w: connector, root, account, and actor values cannot contain NUL", ErrExternalRootValidation)
	}
	if params.ReceiveCommentsAfter.IsZero() {
		return fmt.Errorf("%w: receive-comments frontier is required", ErrExternalRootValidation)
	}
	hasInitialClaimToken := strings.TrimSpace(params.InitialClaimToken) != ""
	hasInitialClaimStartedAt := !params.InitialClaimStartedAt.IsZero()
	if hasInitialClaimToken != hasInitialClaimStartedAt {
		return fmt.Errorf("%w: initial claim token and timestamp must be supplied together", ErrExternalRootValidation)
	}
	if params.UseLocalPublishFrontier && !params.PublishComments {
		return fmt.Errorf("%w: local publish frontier requires publishing", ErrExternalRootValidation)
	}
	if len(params.InitialCommentRevisions) > 0 && !params.UseCommentIdentityFrontier {
		return fmt.Errorf("%w: initial comment revisions require an identity frontier", ErrExternalRootValidation)
	}
	seenCommentIDs := make(map[string]bool, len(params.InitialCommentRevisions))
	for _, comment := range params.InitialCommentRevisions {
		if strings.TrimSpace(comment.ExternalID) == "" || strings.TrimSpace(comment.ExternalID) != comment.ExternalID ||
			strings.TrimSpace(comment.Revision) == "" || strings.TrimSpace(comment.Revision) != comment.Revision ||
			containsNUL(comment.ExternalID, comment.Revision) {
			return fmt.Errorf("%w: initial comment identities and revisions must be canonical", ErrExternalRootValidation)
		}
		if seenCommentIDs[comment.ExternalID] {
			return fmt.Errorf("%w: initial comment identities must be unique", ErrExternalRootValidation)
		}
		seenCommentIDs[comment.ExternalID] = true
	}
	if params.PublishComments {
		if params.UseLocalPublishFrontier {
			if params.PublishCommentsAfter != nil {
				return fmt.Errorf("%w: publish-comments frontier is ambiguous", ErrExternalRootValidation)
			}
		} else if params.PublishCommentsAfter == nil {
			// The zero time is valid here: it records a marker-governed
			// frontier restored from an export, under which outbound
			// publication is decided by durable comment mappings alone.
			return fmt.Errorf("%w: publish-comments frontier is required when publishing is enabled", ErrExternalRootValidation)
		}
	}
	return nil
}

func containsNUL(values ...string) bool {
	for _, value := range values {
		if strings.ContainsRune(value, '\x00') {
			return true
		}
	}
	return false
}

// NormalizeExternalFieldMappingParams validates an enabled bidirectional
// mapping and returns a deterministic accepted-kind ordering for comparisons.
func NormalizeExternalFieldMappingParams(params ExternalFieldMappingParams) (ExternalFieldMappingParams, error) {
	params.ConnectorInstance = strings.TrimSpace(params.ConnectorInstance)
	params.KataField = strings.TrimSpace(params.KataField)
	params.ExternalFieldID = strings.TrimSpace(params.ExternalFieldID)
	params.ExternalFieldName = strings.TrimSpace(params.ExternalFieldName)
	params.SchemaRevision = strings.TrimSpace(params.SchemaRevision)
	if params.ConnectorInstance == "" || params.ExternalFieldID == "" ||
		params.ExternalFieldName == "" || params.SchemaRevision == "" {
		return ExternalFieldMappingParams{}, fmt.Errorf("%w: mapping identities are required", ErrExternalFieldMappingValidation)
	}
	if params.KataField != "scheduled_on" && params.KataField != "deadline_on" {
		return ExternalFieldMappingParams{}, fmt.Errorf("%w: unsupported Kata field %q", ErrExternalFieldMappingValidation, params.KataField)
	}
	if !params.Nullable || !params.Writable {
		return ExternalFieldMappingParams{}, fmt.Errorf("%w: mapped field must be nullable and writable", ErrExternalFieldMappingValidation)
	}
	if len(params.AcceptedKinds) == 0 {
		return ExternalFieldMappingParams{}, fmt.Errorf("%w: accepted kinds are required", ErrExternalFieldMappingValidation)
	}
	seen := make(map[string]struct{}, len(params.AcceptedKinds))
	kinds := make([]string, 0, len(params.AcceptedKinds))
	for _, kind := range params.AcceptedKinds {
		kind = strings.TrimSpace(kind)
		switch kind {
		case "date", "local_datetime", "instant":
		default:
			return ExternalFieldMappingParams{}, fmt.Errorf("%w: unsupported accepted kind %q", ErrExternalFieldMappingValidation, kind)
		}
		if _, duplicate := seen[kind]; duplicate {
			return ExternalFieldMappingParams{}, fmt.Errorf("%w: duplicate accepted kind %q", ErrExternalFieldMappingValidation, kind)
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	params.AcceptedKinds = kinds
	return params, nil
}

// NormalizeExternalFieldMappingExport applies the same descriptor rules used
// by live mapping changes before a snapshot row reaches either backend.
func NormalizeExternalFieldMappingExport(mapping ExternalFieldMappingExport) (ExternalFieldMappingExport, error) {
	if mapping.CreatedAt.IsZero() || mapping.UpdatedAt.IsZero() {
		return ExternalFieldMappingExport{}, fmt.Errorf(
			"%w: mapping timestamps are required", ErrExternalFieldMappingValidation,
		)
	}
	normalized, err := NormalizeExternalFieldMappingParams(ExternalFieldMappingParams{
		ConnectorInstance: mapping.ConnectorInstance,
		KataField:         mapping.KataField,
		ExternalFieldID:   mapping.ExternalFieldID,
		ExternalFieldName: mapping.ExternalFieldName,
		AcceptedKinds:     mapping.AcceptedKinds,
		Nullable:          mapping.Nullable,
		Writable:          mapping.Writable,
		SchemaRevision:    mapping.SchemaRevision,
	})
	if err != nil {
		return ExternalFieldMappingExport{}, err
	}
	mapping.ConnectorInstance = normalized.ConnectorInstance
	mapping.KataField = normalized.KataField
	mapping.ExternalFieldID = normalized.ExternalFieldID
	mapping.ExternalFieldName = normalized.ExternalFieldName
	mapping.AcceptedKinds = normalized.AcceptedKinds
	mapping.SchemaRevision = normalized.SchemaRevision
	return mapping, nil
}

// ValidateExternalFieldStateParams validates a field baseline or conflict update.
func ValidateExternalFieldStateParams(params ExternalFieldStateParams) error {
	if params.BindingID <= 0 || params.MappingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" {
		return fmt.Errorf("%w: binding, mapping, and claim token are required", ErrExternalRootValidation)
	}
	if params.At.IsZero() || strings.TrimSpace(params.Actor) == "" {
		return fmt.Errorf("%w: timestamp and actor are required", ErrExternalRootValidation)
	}
	if err := validateOptionalJSON("baseline", params.Baseline); err != nil {
		return err
	}
	if params.Conflicted {
		if len(params.ConflictKata) == 0 || len(params.ConflictExternal) == 0 {
			return fmt.Errorf("%w: both conflict candidates are required", ErrExternalRootValidation)
		}
		if err := validateOptionalJSON("Kata conflict", params.ConflictKata); err != nil {
			return err
		}
		if err := validateOptionalJSON("external conflict", params.ConflictExternal); err != nil {
			return err
		}
	} else if len(params.ConflictKata) != 0 || len(params.ConflictExternal) != 0 {
		return fmt.Errorf("%w: conflict candidates require conflicted state", ErrExternalRootValidation)
	}
	return nil
}

// ValidateExternalFieldStateExport rejects snapshot state that could not have
// been produced by the claim-guarded runtime mutation path.
func ValidateExternalFieldStateExport(state ExternalFieldStateExport) error {
	if strings.TrimSpace(state.BindingUID) == "" ||
		strings.TrimSpace(state.MappingConnectorInstance) == "" ||
		strings.TrimSpace(state.MappingKataField) == "" ||
		strings.TrimSpace(state.MappingExternalFieldID) == "" ||
		strings.TrimSpace(state.MappingSchemaRevision) == "" ||
		state.MappingCreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: field state identities and timestamps are required", ErrExternalRootValidation)
	}
	if err := validateOptionalJSON("baseline", state.Baseline); err != nil {
		return err
	}
	if state.Conflicted {
		if len(state.ConflictKata) == 0 || len(state.ConflictExternal) == 0 ||
			state.ConflictAt == nil || state.ConflictAt.IsZero() {
			return fmt.Errorf("%w: conflict candidates and timestamp are required", ErrExternalRootValidation)
		}
		if err := validateOptionalJSON("Kata conflict", state.ConflictKata); err != nil {
			return err
		}
		if err := validateOptionalJSON("external conflict", state.ConflictExternal); err != nil {
			return err
		}
	} else if len(state.ConflictKata) != 0 || len(state.ConflictExternal) != 0 || state.ConflictAt != nil {
		return fmt.Errorf("%w: conflict details require conflicted state", ErrExternalRootValidation)
	}
	return nil
}

// ValidateExternalFieldProjectionParams validates a closed native metadata patch.
func ValidateExternalFieldProjectionParams(params ExternalFieldProjectionParams) error {
	if params.BindingID <= 0 || params.MappingID <= 0 || params.ExpectedIssueRevision <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.IntegrationActor) == "" {
		return fmt.Errorf("%w: binding, mapping, issue revision, claim token, and integration actor are required", ErrExternalRootValidation)
	}
	if params.KataField != strings.TrimSpace(params.KataField) {
		return fmt.Errorf("%w: Kata field must be canonical", ErrExternalRootValidation)
	}
	if params.KataField != "scheduled_on" && params.KataField != "deadline_on" {
		return fmt.Errorf("%w: unsupported Kata field %q", ErrExternalRootValidation, params.KataField)
	}
	if len(params.Patch) == 0 || len(params.Patch) > 2 {
		return fmt.Errorf("%w: field projection patch is invalid", ErrExternalRootValidation)
	}
	if _, ok := params.Patch[params.KataField]; !ok {
		return fmt.Errorf("%w: field projection patch must contain %s", ErrExternalRootValidation, params.KataField)
	}
	for key, raw := range params.Patch {
		if key != params.KataField && key != "timezone" {
			return fmt.Errorf("%w: field projection patch contains unsupported key %q", ErrExternalRootValidation, key)
		}
		if !json.Valid(raw) {
			return fmt.Errorf("%w: field projection patch contains invalid JSON", ErrExternalRootValidation)
		}
	}
	return nil
}

// ValidateResolveExternalFieldConflictParams validates explicit conflict resolution.
func ValidateResolveExternalFieldConflictParams(params ResolveExternalFieldConflictParams) error {
	if params.BindingID <= 0 || params.MappingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.Actor) == "" || params.At.IsZero() {
		return fmt.Errorf("%w: binding, mapping, claim token, actor, and timestamp are required", ErrExternalRootValidation)
	}
	return validateOptionalJSON("baseline", params.Baseline)
}

func validateOptionalJSON(name string, raw json.RawMessage) error {
	if len(raw) != 0 && !json.Valid(raw) {
		return fmt.Errorf("%w: %s is invalid JSON", ErrExternalRootValidation, name)
	}
	return nil
}

var externalRootCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)[^,;\r\n]+`),
	regexp.MustCompile(`(?i)\b(password|passwd|token|secret|api[_-]?key|credential)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`://[^/@\s]+@`),
}

// SafeExternalRootError applies a defense-in-depth credential scrub and caps
// persisted diagnostics at 2 KiB. Connector clients remain responsible for
// redacting configured secret values before returning errors to storage.
func SafeExternalRootError(message string) string {
	message = externalRootCredentialPatterns[0].ReplaceAllString(message, `${1}[redacted]`)
	message = externalRootCredentialPatterns[1].ReplaceAllStringFunc(message, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		if separator < 0 {
			return "[redacted]"
		}
		return strings.TrimSpace(match[:separator]) + string(match[separator]) + "[redacted]"
	})
	message = externalRootCredentialPatterns[2].ReplaceAllString(message, `://[redacted]@`)
	const limit = 2048
	if len(message) <= limit {
		return message
	}
	message = message[:limit]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}

// IsExternalRootAuditEvent reports whether an event is a bridge audit mutation.
func IsExternalRootAuditEvent(eventType string) bool {
	switch eventType {
	case "issue.external_root_bound", "issue.external_root_paused",
		"issue.external_root_resumed", "issue.external_root_unbound",
		"issue.external_comment_resolved", "issue.external_field_conflicted",
		"issue.external_field_resolved":
		return true
	default:
		return false
	}
}
