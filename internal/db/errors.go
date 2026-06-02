package db

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors exposed by the Storage contract. The daemon and CLI match
// these with errors.Is; new sentinels live here so impl files stay method-only.

// Issue/project core sentinels.
var (
	// ErrNotFound is returned when a referenced row does not exist (or has
	// been soft-deleted and the lookup did not opt in to deleted rows).
	ErrNotFound = errors.New("not found")

	// ErrOpenChildren is returned when closing a parent issue while it still
	// has open child issues.
	ErrOpenChildren = errors.New("issue has open children")

	// ErrNoFields is returned by EditIssue when no field changes are
	// requested.
	ErrNoFields = errors.New("no fields to update")

	// ErrInitialLinkTargetNotFound is returned when CreateIssue's InitialLink
	// references a parent issue that does not exist.
	ErrInitialLinkTargetNotFound = errors.New("initial link target not found")

	// ErrInitialLinkInvalidType is returned when CreateIssue's InitialLink
	// names a relation other than "parent_of".
	ErrInitialLinkInvalidType = errors.New("invalid initial link type")

	// ErrAlreadyClaimed is returned by ClaimOwner when another actor already
	// owns the issue and Force=false.
	ErrAlreadyClaimed = errors.New("already claimed")
)

// EditIssueAtomic sentinels.
var (
	// ErrParentMismatch is returned by EditIssueAtomic when RemoveParent's
	// asserted number does not match the current parent (including the
	// no-parent case). Surfaced by the handler as 409 parent_mismatch.
	ErrParentMismatch = errors.New("parent mismatch")

	// ErrParentCycle is returned when set_parent would create a cycle in the
	// parent graph (the new parent is already a descendant of the issue under
	// edit). Surfaced by the handler as 400 validation.
	ErrParentCycle = errors.New("parent cycle")
)

// Link sentinels.
var (
	// ErrLinkExists is returned when CreateLink tries to add a duplicate edge.
	ErrLinkExists = errors.New("link already exists")
	// ErrParentAlreadySet is returned when CreateLink would set a second parent
	// on a child.
	ErrParentAlreadySet = errors.New("parent already set")
	// ErrSelfLink is returned when CreateLink would link an issue to itself.
	ErrSelfLink = errors.New("self-link not allowed")
	// ErrCrossProjectLink is returned when CreateLink endpoints span projects.
	ErrCrossProjectLink = errors.New("cross-project link not allowed")
)

// Label sentinels.
var (
	// ErrLabelExists is returned by AddLabel when the (issue, label) pair is
	// already present.
	ErrLabelExists = errors.New("label already attached")
	// ErrLabelInvalid is returned when a label string fails validation.
	ErrLabelInvalid = errors.New("invalid label")
)

// Recurrence sentinels.
var (
	// ErrInvalidRecurrence is returned when a recurrence rule fails validation.
	ErrInvalidRecurrence = errors.New("invalid recurrence")
)

// Project archive / alias / merge sentinels.
var (
	// ErrProjectAlreadyArchived is returned when RemoveProject is called on a
	// project whose deleted_at is already set.
	ErrProjectAlreadyArchived = errors.New("project already archived")

	// ErrProjectHasOpenIssues is returned when RemoveProject is called without
	// Force on a project that still has at least one open, non-deleted issue.
	ErrProjectHasOpenIssues = errors.New("project has open issues")

	// ErrAliasIsLast is returned when DetachProjectAlias is called without
	// Force on the only remaining alias for a project.
	ErrAliasIsLast = errors.New("alias is the last alias for its project")

	// ErrProjectMergeSameProject is returned when source and target are the
	// same project row.
	ErrProjectMergeSameProject = errors.New("cannot merge a project into itself")

	// ErrProjectMergeImportMappingCollision is returned when moving source import
	// mappings would violate the target's source identity uniqueness.
	ErrProjectMergeImportMappingCollision = errors.New("project merge import mapping collision")

	// ErrProjectMergeArchivedSource is returned when MergeProjects is asked
	// to merge from a project that's been archived via RemoveProject (#24).
	ErrProjectMergeArchivedSource = errors.New("source project is archived")

	// ErrProjectMergeArchivedTarget is returned when the target project is
	// archived.
	ErrProjectMergeArchivedTarget = errors.New("target project is archived")

	// ErrProjectMergeFederationBinding is returned when either side of a
	// project merge has a federation binding.
	ErrProjectMergeFederationBinding = errors.New("project merge federation binding")
)

// Import sentinel.
var (
	// ErrImportValidation is returned by ImportBatch when the request fails
	// validation (missing fields, bad status, unresolved link target).
	ErrImportValidation = errors.New("invalid import")
)

// Claim sentinels.
var (
	// ErrClaimRequired reports that an issue mutation needs a live claim.
	ErrClaimRequired = errors.New("claim required")
	// ErrClaimDenied reports that another holder owns the relevant claim.
	ErrClaimDenied = errors.New("claim denied")
	// ErrClaimNotHeld reports release or renew by a non-holder.
	ErrClaimNotHeld = errors.New("claim not held")
	// ErrClaimExpired reports that the caller's matching timed claim is stale.
	ErrClaimExpired = errors.New("claim expired")
	// ErrClaimValidation reports an invalid claim request or state transition.
	ErrClaimValidation = errors.New("claim validation")
	// ErrPendingClaimNotAuthoritative reports that a pending claim cannot authorize work.
	ErrPendingClaimNotAuthoritative = errors.New("pending claim not authoritative")
)

// Federation sentinels.
var (
	// ErrRemoteEventHashMismatch reports a remote event whose advertised content
	// hash does not match the portable event fields.
	ErrRemoteEventHashMismatch = errors.New("remote event content hash mismatch")

	// ErrRemoteEventConflict reports a duplicate remote event UID with different
	// content than the row already stored locally.
	ErrRemoteEventConflict = errors.New("remote event uid conflict")

	// ErrFederatedReadOnly is returned when a local mutation targets an enabled
	// spoke replica whose local writes are not available for that operation.
	ErrFederatedReadOnly = errors.New("federated spoke project is read-only")

	// ErrFederatedSpokeUnsupported is joined with ErrFederatedReadOnly for Phase 2
	// operations that remain hub-only even when a spoke is push-enabled.
	ErrFederatedSpokeUnsupported = errors.New("federated spoke operation unsupported")

	// ErrFederatedMoveUnsupported is returned when a move involves any federated
	// project. Cross-project federated move semantics need a separate design.
	ErrFederatedMoveUnsupported = errors.New("federated project move unsupported")

	// ErrFederationResetBlockedByPendingPush is returned when a federation reset
	// is blocked because there are pending push events.
	ErrFederationResetBlockedByPendingPush = errors.New("federation reset blocked by pending push")

	// ErrFederationPushQuarantined reports that the federation push channel is
	// quarantined.
	ErrFederationPushQuarantined = errors.New("federation push quarantined")

	// ErrFederationResetBlockedByQuarantine is returned when a federation reset
	// cannot proceed due to an active quarantine.
	ErrFederationResetBlockedByQuarantine = errors.New("federation reset blocked by quarantine")

	// ErrFederationIngestValidation is returned by IngestFederationEvents when a
	// batch fails validation.
	ErrFederationIngestValidation = errors.New("federation ingest validation")
)

// LinkTargetNotFoundError carries the offending project-scoped number
// when an add-edge or set-parent operation references an issue that
// doesn't exist. The handler renders a message that names the
// specific ref so multi-flag PATCHes (`--blocks 5 --blocks 99 --blocks 7`)
// can identify which target failed. Wraps ErrNotFound so existing
// errors.Is checks keep working.
type LinkTargetNotFoundError struct {
	Number int64
}

func (e *LinkTargetNotFoundError) Error() string {
	return fmt.Sprintf("link target #%d not found", e.Number)
}

func (e *LinkTargetNotFoundError) Unwrap() error { return ErrNotFound }

// ProjectHasOpenIssuesError carries the open-issue count alongside the
// sentinel error so handlers can format the refusal message with the actual
// number ("3 open issues remain").
type ProjectHasOpenIssuesError struct {
	OpenIssues int64
}

func (e *ProjectHasOpenIssuesError) Error() string {
	return fmt.Sprintf("%v: %d", ErrProjectHasOpenIssues, e.OpenIssues)
}

func (e *ProjectHasOpenIssuesError) Unwrap() error { return ErrProjectHasOpenIssues }

// ProjectMergeImportMappingCollision identifies one import mapping identity
// that already exists on the target project.
type ProjectMergeImportMappingCollision struct {
	Source     string
	ExternalID string
	ObjectType string
}

// ProjectMergeImportMappingCollisionError carries the import mapping
// identities that blocked a merge.
type ProjectMergeImportMappingCollisionError struct {
	Mappings []ProjectMergeImportMappingCollision
}

func (e *ProjectMergeImportMappingCollisionError) Error() string {
	parts := make([]string, 0, len(e.Mappings))
	for _, m := range e.Mappings {
		parts = append(parts, fmt.Sprintf("%s/%s/%s", m.Source, m.ObjectType, m.ExternalID))
	}
	return fmt.Sprintf("%v: %s", ErrProjectMergeImportMappingCollision, strings.Join(parts, ", "))
}

func (e *ProjectMergeImportMappingCollisionError) Unwrap() error {
	return ErrProjectMergeImportMappingCollision
}

// CrossProjectLinksError is returned by MoveIssueProject when the issue
// has one or more links that would become cross-project after the move.
type CrossProjectLinksError struct {
	Blockers []LinkBlocker
}

func (e *CrossProjectLinksError) Error() string {
	return fmt.Sprintf("cannot move: %d cross-project link(s) anchored in source project",
		len(e.Blockers))
}

// LinkBlocker identifies one link that prevents a project move.
type LinkBlocker struct {
	LinkID  int64  `json:"link_id"`
	PeerUID string `json:"peer_uid"`
	Type    string `json:"type"`
}

// RecurrencePinnedError is returned by MoveIssueProject when the issue
// is part of a recurrence series (recurrence_id IS NOT NULL).
type RecurrencePinnedError struct{}

func (e *RecurrencePinnedError) Error() string {
	return "cannot move: issue is part of a recurrence series"
}

// RevisionConflictError is returned by MoveIssueProject when the caller's
// IfMatchRev does not match the issue's current revision.
type RevisionConflictError struct {
	CurrentRevision int64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("revision conflict: current revision is %d", e.CurrentRevision)
}
