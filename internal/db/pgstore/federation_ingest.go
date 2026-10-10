package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

type preparedFederationIngestEvent struct {
	sourceEventID int64
	event         db.RemoteEvent
	duplicate     bool
}

// IngestFederationEvents validates, stores, and materializes one spoke batch atomically.
func (s *Store) IngestFederationEvents(
	ctx context.Context,
	params db.FederationIngestParams,
) (db.FederationIngestResult, error) {
	if len(params.Events) == 0 {
		return s.negotiateEmptyFederationIngest(ctx, params)
	}
	var result db.FederationIngestResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.FederationIngestResult{}
		projectUID, projectName, err := requireFederationIngestHub(ctx, tx, params.ProjectID, true)
		if err != nil {
			return err
		}
		required, err := db.ProjectRequiredEventFeatures(ctx, tx, projectUID)
		if err != nil {
			return err
		}
		if err := db.RequireEventFeatures(params.EventFeatures, required); err != nil {
			return err
		}
		result.RequiredEventFeatures = required
		if err := db.LockCronProject(ctx, tx, params.ProjectID); err != nil {
			return err
		}
		for _, input := range params.Events {
			if err := db.RequireEventFeatures(params.EventFeatures, db.EventRequiredFeatures(input.Event.Type)); err != nil {
				return err
			}
			if feature := db.EventRequiredFeatures(input.Event.Type); feature != "" {
				result.RequiredEventFeatures = feature
			}
		}
		knownIssueUIDs, err := currentFederatedIssueUIDSet(ctx, tx, params.ProjectID)
		if err != nil {
			return err
		}
		adoptionState, err := computeFederationIngestAdoptionSnapshotAuthorState(ctx, tx,
			params.ProjectID, params.FederationEnrollmentID, params.SpokeInstanceUID,
			params.AllowSnapshotAuthorPreservation, params.AdoptionBaseline,
			params.AdoptionBaselineEndSourceEventID, params.Events)
		if err != nil {
			return err
		}
		prepared := make([]preparedFederationIngestEvent, 0, len(params.Events))
		seenBatch := map[string]string{}
		freshSnapshotSeen := false
		boundActor := strings.TrimSpace(params.BoundActor)
		for _, input := range params.Events {
			if input.SourceEventID <= 0 {
				return fmt.Errorf("%w: source event id must be positive", db.ErrFederationIngestValidation)
			}
			if input.SourceEventID > result.PushCursorEventID {
				result.PushCursorEventID = input.SourceEventID
			}
			event := input.Event
			if len(event.Payload) == 0 {
				event.Payload = jsontext.Value(`{}`)
			}
			if err := validateFederationProjectEvent(
				projectUID, params.SpokeInstanceUID, event, knownIssueUIDs,
			); err != nil {
				return err
			}
			if boundActor != "" && event.Actor != boundActor {
				return fmt.Errorf("%w: event %s actor %q does not match bound actor",
					db.ErrFederationIngestValidation, event.EventUID, event.Actor)
			}
			if _, _, err := db.ValidateRemoteEventContentHash(event); err != nil {
				return err
			}
			if existingHash, ok := seenBatch[event.EventUID]; ok {
				matches, err := federationEventHashMatches(
					event, existingHash, boundActor, adoptionState.overrideSnapshotAuthors,
				)
				if err != nil {
					return err
				}
				if !matches {
					return fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, event.EventUID)
				}
				result.Duplicates++
				prepared = append(prepared, preparedFederationIngestEvent{
					sourceEventID: input.SourceEventID, event: event, duplicate: true,
				})
				continue
			}
			existingHash, err := federationEventHashByUID(ctx, tx, event.EventUID)
			if err == nil {
				matches, matchErr := federationEventHashMatches(
					event, existingHash, boundActor, adoptionState.overrideSnapshotAuthors,
				)
				if matchErr != nil {
					return matchErr
				}
				if !matches {
					return fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, event.EventUID)
				}
				result.Duplicates++
				rememberIngestIssueUIDs(event, knownIssueUIDs)
				prepared = append(prepared, preparedFederationIngestEvent{
					sourceEventID: input.SourceEventID, event: event, duplicate: true,
				})
				continue
			}
			if !errors.Is(err, db.ErrNotFound) {
				return err
			}
			if adoptionState.duplicateOnly {
				return fmt.Errorf("%w: adoption baseline retry contains fresh event %s",
					db.ErrFederationIngestValidation, event.EventUID)
			}
			if adoptionState.overrideSnapshotAuthors {
				event, err = db.CanonicalizeFederationSnapshotAuthors(event, boundActor)
				if err != nil {
					return err
				}
			}
			if err := db.ValidateFederationBoundActorPayload(ctx, event, db.FederationBoundActorCheck{
				BoundActor:                      boundActor,
				AllowSnapshotAuthorPreservation: adoptionState.allowAuthorPreservation,
				AdoptionBaseline:                adoptionState.allowAuthorPreservation || adoptionState.overrideSnapshotAuthors,
				Known:                           db.FederationAttributionSQL{Tx: tx, ProjectID: params.ProjectID},
			}); err != nil {
				return err
			}
			if freshSnapshotSeen && !db.IsFederationSnapshotEvent(event.Type) {
				return fmt.Errorf("%w: non-snapshot event %s follows snapshot baseline in same batch",
					db.ErrFederationIngestValidation, event.EventUID)
			}
			if err := rejectFreshCreateSnapshotForKnownIssue(event, knownIssueUIDs); err != nil {
				return err
			}
			if db.IsFederationSnapshotEvent(event.Type) {
				freshSnapshotSeen = true
			}
			seenBatch[event.EventUID] = event.ContentHash
			rememberIngestIssueUIDs(event, knownIssueUIDs)
			prepared = append(prepared, preparedFederationIngestEvent{
				sourceEventID: input.SourceEventID, event: event,
			})
		}
		cronValidator := db.NewCronReplayValidator(tx, true)
		cronTargets := make([]db.RemoteEvent, 0, len(prepared))
		for _, input := range prepared {
			if !input.duplicate {
				cronTargets = append(cronTargets, input.event)
			}
		}
		if err := cronValidator.PrepareEvents(ctx, params.ProjectID, cronTargets); err != nil {
			return err
		}

		linksAffected := false
		for _, input := range prepared {
			if input.duplicate {
				continue
			}
			inserted, err := s.insertFederationEventTx(
				ctx, tx, params.ProjectID, projectName, input.event, cronValidator,
			)
			if err != nil {
				return err
			}
			if !inserted {
				result.Duplicates++
				continue
			}
			result.Accepted++
			if db.FederationEventAffectsLinks(input.event.Type) {
				linksAffected = true
			}
			result.InsertedEventUIDs = append(result.InsertedEventUIDs, input.event.EventUID)
			auditEvents, err := s.annotateFederationIngestClaimWorkTx(
				ctx, tx, params.ProjectID, input.event,
			)
			if err != nil {
				return err
			}
			result.Events = append(result.Events, auditEvents...)
			for _, auditEvent := range auditEvents {
				result.InsertedEventUIDs = append(result.InsertedEventUIDs, auditEvent.UID)
			}
		}
		if result.Accepted > 0 {
			// The generated claim audit events are never link-bearing, so the
			// accepted batch alone decides whether the binding-group link fold
			// has any work to do.
			if err := s.materializeFederatedProjectTx(ctx, tx, params.ProjectID, linksAffected, result.InsertedEventUIDs, cronValidator); err != nil {
				return err
			}

			if !adoptionState.shouldDeferMarker {
				err = consumeFederationAdoptionSnapshotAuthorMarker(ctx, tx, params.ProjectID, params.FederationEnrollmentID, params.SpokeInstanceUID)
			} else {
				err = recordFederationAdoptionBaselineProgress(ctx, tx, params.ProjectID, params.FederationEnrollmentID, params.SpokeInstanceUID, adoptionState.nextSourceEventID, adoptionState.endSourceEventID, adoptionState.deferAuthorPreservationGrant)
			}
			if err != nil {
				return err
			}
		}
		result.InsertedEventUIDs, result.Events, err = db.RetainFederationEvents(ctx, result.InsertedEventUIDs, result.Events, func(ctx context.Context, ids []string) ([]db.Event, error) {
			return eventsByUIDs(ctx, tx, params.ProjectID, ids)
		})
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) insertFederationEventTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectName string,
	event db.RemoteEvent,
	validator *db.CronReplayValidator,
) (bool, error) {
	if err := validator.Validate(ctx, projectID, event); err != nil {
		return false, err
	}
	storedProjectName := event.ProjectName
	if storedProjectName == "" {
		storedProjectName = projectName
	}
	clock := db.EventHLCTimestamp{PhysicalMS: event.HLCPhysicalMS, Counter: event.HLCCounter}
	_, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: projectID, ProjectUID: event.ProjectUID, ProjectName: storedProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, Payload: string(event.Payload),
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, HLC: &clock,
		CreatedAt: formatStoredTime(event.CreatedAt), ContentHash: event.ContentHash,
	})
	if err == nil {
		return true, nil
	}
	return false, err
}

// negotiateEmptyFederationIngest answers a push that carries no events. It
// writes nothing, so it uses plain reads outside any transaction: no
// enrollment fence or row lock makes it wait behind a writer. The route has
// already authenticated the caller. It still refuses a peer that lacks the
// event features the project's history requires.
func (s *Store) negotiateEmptyFederationIngest(ctx context.Context, params db.FederationIngestParams) (db.FederationIngestResult, error) {
	projectUID, _, err := requireFederationIngestHub(ctx, s, params.ProjectID, false)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	required, err := db.ProjectRequiredEventFeatures(ctx, s, projectUID)
	if err != nil {
		return db.FederationIngestResult{}, mapSQLError(err, nil)
	}
	if err := db.RequireEventFeatures(params.EventFeatures, required); err != nil {
		return db.FederationIngestResult{}, err
	}
	return db.FederationIngestResult{RequiredEventFeatures: required}, nil
}

// requireFederationIngestHub confirms the project is an enabled hub. A batch
// that writes locks the project and binding rows; an empty batch only reads
// them.
func requireFederationIngestHub(
	ctx context.Context,
	q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	projectID int64,
	lock bool,
) (string, string, error) {
	var projectUID, projectName, role string
	var enabled int
	query := `SELECT p.uid,p.name,fb.role,fb.enabled
FROM projects p JOIN federation_bindings fb ON fb.project_id=p.id
WHERE p.id=$1 AND p.deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE OF p,fb`
	}
	err := q.QueryRowContext(ctx, query, projectID).
		Scan(&projectUID, &projectName, &role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", db.ErrNotFound
	}
	if err != nil {
		return "", "", mapSQLError(err, nil)
	}
	if role != string(db.FederationRoleHub) || enabled != 1 {
		return "", "", fmt.Errorf("%w: project is not an enabled federation hub",
			db.ErrFederationIngestValidation)
	}
	return projectUID, projectName, nil
}

func federationEventHashByUID(ctx context.Context, tx *sql.Tx, eventUID string) (string, error) {
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE uid=$1`, eventUID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ErrNotFound
	}
	if err != nil {
		return "", mapSQLError(err, nil)
	}
	return hash, nil
}

func validateFederationProjectEvent(
	projectUID string,
	spokeInstanceUID string,
	event db.RemoteEvent,
	knownIssueUIDs map[string]struct{},
) error {
	if event.ProjectUID != projectUID {
		return fmt.Errorf("%w: event %s targets project %s",
			db.ErrFederationIngestValidation, event.EventUID, event.ProjectUID)
	}
	if event.OriginInstanceUID != spokeInstanceUID {
		return fmt.Errorf("%w: event %s origin mismatch", db.ErrFederationIngestValidation, event.EventUID)
	}
	if event.EventUID == "" || event.HLCPhysicalMS <= 0 || event.HLCCounter < 0 ||
		strings.TrimSpace(event.Actor) == "" {
		return fmt.Errorf("%w: event %s has invalid envelope",
			db.ErrFederationIngestValidation, event.EventUID)
	}
	if strings.HasPrefix(event.Type, "recurrence.") || event.Type == "issue.moved" {
		return fmt.Errorf("%w: event type %s unsupported in phase 2",
			db.ErrFederationIngestValidation, event.Type)
	}
	if strings.HasPrefix(event.Type, "cron.") {
		return db.ValidateCronFederationEvent(event)
	}
	payload := db.PayloadMap(event.Payload)
	if event.Type == "project.metadata_updated" {
		if payloadProjectUID, ok := db.StringValue(payload["project_uid"]); ok && payloadProjectUID != projectUID {
			return fmt.Errorf("%w: project metadata payload targets %s",
				db.ErrFederationIngestValidation, payloadProjectUID)
		}
		return nil
	}
	issueUID, err := payloadIssueUID(event, payload)
	if err != nil {
		return err
	}
	switch event.Type {
	case "issue.created", "issue.snapshot":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", db.ErrFederationIngestValidation, event.Type)
		}
	case "issue.updated", "issue.assigned", "issue.unassigned",
		"issue.assignment_renewed", "issue.assignment_expired",
		"issue.priority_set", "issue.priority_cleared", "issue.closed", "issue.reopened",
		"issue.soft_deleted", "issue.restored", "issue.commented", "issue.comment_edited",
		"issue.labeled", "issue.unlabeled", "issue.linked", "issue.unlinked",
		"issue.links_changed", "issue.metadata_updated",
		"issue.external_root_bound", "issue.external_root_paused",
		"issue.external_root_resumed", "issue.external_root_unbound",
		"issue.external_comment_resolved", "issue.external_field_conflicted",
		"issue.external_field_resolved":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", db.ErrFederationIngestValidation, event.Type)
		}
		if _, ok := knownIssueUIDs[issueUID]; !ok {
			return fmt.Errorf("%w: %s references unknown issue %s",
				db.ErrFederationIngestValidation, event.Type, issueUID)
		}
	default:
		return fmt.Errorf("%w: unsupported event type %s", db.ErrFederationIngestValidation, event.Type)
	}
	deferred, err := payloadDeferredLinkIssueUIDs(event, payload, issueUID)
	if err != nil {
		return err
	}
	references, err := payloadReferencedIssueUIDs(event, payload)
	if err != nil {
		return err
	}
	for _, reference := range references {
		if reference == issueUID {
			continue
		}
		if _, ok := knownIssueUIDs[reference]; ok {
			continue
		}
		if _, ok := deferred[reference]; ok {
			continue
		}
		return fmt.Errorf("%w: event %s references unknown issue %s",
			db.ErrFederationIngestValidation, event.EventUID, reference)
	}
	return nil
}

func rejectFreshCreateSnapshotForKnownIssue(
	event db.RemoteEvent,
	knownIssueUIDs map[string]struct{},
) error {
	if event.Type != "issue.created" && event.Type != "issue.snapshot" {
		return nil
	}
	issueUID, err := payloadIssueUID(event, db.PayloadMap(event.Payload))
	if err != nil {
		return err
	}
	if _, ok := knownIssueUIDs[issueUID]; ok {
		return fmt.Errorf("%w: fresh %s targets existing issue %s",
			db.ErrFederationIngestValidation, event.Type, issueUID)
	}
	return nil
}

func payloadIssueUID(event db.RemoteEvent, payload map[string]jsontext.Value) (string, error) {
	payloadUID, _ := db.StringValue(payload["issue_uid"])
	if value, ok := db.StringValue(payload["uid"]); ok {
		if payloadUID != "" && payloadUID != value {
			return "", fmt.Errorf("%w: payload issue uid disagreement", db.ErrFederationIngestValidation)
		}
		payloadUID = value
	}
	if event.IssueUID != nil {
		if payloadUID != "" && payloadUID != *event.IssueUID {
			return "", fmt.Errorf("%w: envelope/payload issue uid disagreement",
				db.ErrFederationIngestValidation)
		}
		return *event.IssueUID, nil
	}
	return payloadUID, nil
}

func payloadReferencedIssueUIDs(
	event db.RemoteEvent,
	payload map[string]jsontext.Value,
) ([]string, error) {
	var references []string
	if event.RelatedIssueUID != nil && *event.RelatedIssueUID != "" {
		references = append(references, *event.RelatedIssueUID)
	}
	for _, key := range []string{"from_uid", "to_uid", "from_issue_uid", "to_issue_uid"} {
		if value, ok := db.StringValue(payload[key]); ok {
			references = append(references, value)
		}
	}
	changed, err := payloadLinksChangedIssueUIDs(event, payload)
	if err != nil {
		return nil, err
	}
	references = append(references, changed...)
	links, err := payloadLinks(event)
	if err != nil {
		return nil, fmt.Errorf("%w: decode links: %v", db.ErrFederationIngestValidation, err)
	}
	for _, link := range links {
		if link.ToIssueUID != "" {
			references = append(references, link.ToIssueUID)
		}
	}
	return references, nil
}

func payloadDeferredLinkIssueUIDs(
	event db.RemoteEvent,
	payload map[string]jsontext.Value,
	primaryIssueUID string,
) (map[string]struct{}, error) {
	deferred := map[string]struct{}{}
	add := func(issueUID string) {
		if issueUID != "" {
			deferred[issueUID] = struct{}{}
		}
	}
	switch event.Type {
	case "issue.created", "issue.snapshot":
		links, err := payloadLinks(event)
		if err != nil {
			return nil, fmt.Errorf("%w: decode links: %v", db.ErrFederationIngestValidation, err)
		}
		for _, link := range links {
			if err := validateFederationLinkType(link.Type); err != nil {
				return nil, err
			}
			if err := validateFederationLinkPeer(primaryIssueUID, link.ToIssueUID); err != nil {
				return nil, err
			}
			if event.Type == "issue.created" && link.Type == "parent" && link.Incoming {
				return nil, fmt.Errorf("%w: parent link cannot be incoming", db.ErrFederationIngestValidation)
			}
			add(link.ToIssueUID)
		}
	case "issue.linked", "issue.unlinked":
		fromUID, fromOK, err := payloadLinkEndpointUID(payload, "from_uid", "from_issue_uid")
		if err != nil {
			return nil, err
		}
		toUID, toOK, err := payloadLinkEndpointUID(payload, "to_uid", "to_issue_uid")
		if err != nil {
			return nil, err
		}
		if !fromOK || !toOK {
			return nil, fmt.Errorf("%w: %s missing link endpoint uid",
				db.ErrFederationIngestValidation, event.Type)
		}
		linkType, ok := db.StringValue(payload["type"])
		if !ok {
			return nil, fmt.Errorf("%w: %s missing link type", db.ErrFederationIngestValidation, event.Type)
		}
		if err := validateFederationLinkType(linkType); err != nil {
			return nil, err
		}
		peerUID := ""
		switch primaryIssueUID {
		case fromUID:
			peerUID = toUID
		case toUID:
			peerUID = fromUID
		default:
			return nil, fmt.Errorf("%w: %s primary issue %s is not a link endpoint",
				db.ErrFederationIngestValidation, event.Type, primaryIssueUID)
		}
		if event.RelatedIssueUID != nil && *event.RelatedIssueUID != peerUID {
			return nil, fmt.Errorf("%w: %s related issue %s is not the opposite endpoint %s",
				db.ErrFederationIngestValidation, event.Type, *event.RelatedIssueUID, peerUID)
		}
		if err := validateFederationLinkPeer(primaryIssueUID, peerUID); err != nil {
			return nil, err
		}
		if err := validateFederationUnlinkStorageEndpoints(
			event, payload, linkType, fromUID, toUID,
		); err != nil {
			return nil, err
		}
		add(peerUID)
	case "issue.links_changed":
		peers, err := payloadLinksChangedIssueUIDs(event, payload)
		if err != nil {
			return nil, err
		}
		for _, peerUID := range peers {
			add(peerUID)
		}
		if event.RelatedIssueUID != nil {
			if _, ok := deferred[*event.RelatedIssueUID]; !ok {
				return nil, fmt.Errorf("%w: %s related issue %s is not a payload peer",
					db.ErrFederationIngestValidation, event.Type, *event.RelatedIssueUID)
			}
		}
		for peerUID := range deferred {
			if err := validateFederationLinkPeer(primaryIssueUID, peerUID); err != nil {
				return nil, err
			}
		}
	}
	return deferred, nil
}

func validateFederationUnlinkStorageEndpoints(
	event db.RemoteEvent,
	payload map[string]jsontext.Value,
	linkType string,
	fromUID string,
	toUID string,
) error {
	if event.Type != "issue.unlinked" {
		return nil
	}
	rawFrom, fromPresent := payload["link_from_uid"]
	rawTo, toPresent := payload["link_to_uid"]
	if fromPresent != toPresent {
		return fmt.Errorf("%w: issue.unlinked storage endpoints must be paired",
			db.ErrFederationIngestValidation)
	}
	if !fromPresent {
		if linkType == "blocks" || linkType == "parent" {
			return fmt.Errorf("%w: issue.unlinked missing directional storage endpoints",
				db.ErrFederationIngestValidation)
		}
		return nil
	}
	linkFromUID, fromOK := db.StringValue(rawFrom)
	linkToUID, toOK := db.StringValue(rawTo)
	if !fromOK || !toOK {
		return fmt.Errorf("%w: issue.unlinked storage endpoints must be strings",
			db.ErrFederationIngestValidation)
	}
	if !katauid.Valid(linkFromUID) || !katauid.Valid(linkToUID) || linkFromUID == linkToUID {
		return fmt.Errorf("%w: issue.unlinked has invalid storage endpoints",
			db.ErrFederationIngestValidation)
	}
	matchesForward := linkFromUID == fromUID && linkToUID == toUID
	matchesReverse := linkFromUID == toUID && linkToUID == fromUID
	if !matchesForward && !matchesReverse {
		return fmt.Errorf("%w: issue.unlinked storage endpoints disagree with payload endpoints",
			db.ErrFederationIngestValidation)
	}
	return nil
}

func payloadLinksChangedIssueUIDs(
	event db.RemoteEvent,
	payload map[string]jsontext.Value,
) ([]string, error) {
	if event.Type != "issue.links_changed" {
		return nil, nil
	}
	var references []string
	for _, key := range []string{"parent_set_uid", "parent_removed_uid"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		var issueUID string
		if err := json.Unmarshal(raw, &issueUID); err != nil {
			return nil, fmt.Errorf("%w: %s must be a string: %v",
				db.ErrFederationIngestValidation, key, err)
		}
		references = append(references, issueUID)
	}
	for _, key := range []string{
		"blocks_added_uids", "blocks_removed_uids", "blocked_by_added_uids",
		"blocked_by_removed_uids", "related_added_uids", "related_removed_uids",
	} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, fmt.Errorf("%w: %s must be an array of strings",
				db.ErrFederationIngestValidation, key)
		}
		var issueUIDs []string
		if err := json.Unmarshal(raw, &issueUIDs); err != nil {
			return nil, fmt.Errorf("%w: %s must be an array of strings: %v",
				db.ErrFederationIngestValidation, key, err)
		}
		references = append(references, issueUIDs...)
	}
	return references, nil
}

func validateFederationLinkType(linkType string) error {
	switch linkType {
	case "parent", "blocks", "related":
		return nil
	default:
		return fmt.Errorf("%w: unsupported link type %q", db.ErrFederationIngestValidation, linkType)
	}
}

func validateFederationLinkPeer(primaryIssueUID, peerUID string) error {
	if !katauid.Valid(peerUID) {
		return fmt.Errorf("%w: invalid link peer uid %q", db.ErrFederationIngestValidation, peerUID)
	}
	if peerUID == primaryIssueUID {
		return fmt.Errorf("%w: issue cannot link to itself", db.ErrFederationIngestValidation)
	}
	return nil
}

func payloadLinkEndpointUID(
	payload map[string]jsontext.Value,
	canonicalKey string,
	alternateKey string,
) (string, bool, error) {
	canonical, canonicalOK := db.StringValue(payload[canonicalKey])
	alternate, alternateOK := db.StringValue(payload[alternateKey])
	if !canonicalOK && alternateOK {
		return "", false, fmt.Errorf("%w: link endpoint must use %s",
			db.ErrFederationIngestValidation, canonicalKey)
	}
	if canonicalOK && alternateOK && canonical != alternate {
		return "", false, fmt.Errorf("%w: link endpoint uid disagreement",
			db.ErrFederationIngestValidation)
	}
	return canonical, canonicalOK, nil
}

type payloadLink struct {
	Type       string `json:"type"`
	ToIssueUID string `json:"to_issue_uid"`
	Incoming   bool   `json:"incoming"`
}

func payloadLinks(event db.RemoteEvent) ([]payloadLink, error) {
	var payload struct {
		Links []payloadLink `json:"links"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, err
	}
	return payload.Links, nil
}

func currentFederatedIssueUIDSet(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (map[string]struct{}, error) {
	output, err := materializedIssueUIDSet(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT issue_uid FROM events
WHERE project_id=$1 AND issue_uid IS NOT NULL AND type IN ('issue.created','issue.snapshot')`,
		projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issueUID string
		if err := rows.Scan(&issueUID); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[issueUID] = struct{}{}
	}
	return output, mapSQLError(rows.Err(), nil)
}

func materializedIssueUIDSet(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uid FROM issues WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[string]struct{}{}
	for rows.Next() {
		var issueUID string
		if err := rows.Scan(&issueUID); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[issueUID] = struct{}{}
	}
	return output, mapSQLError(rows.Err(), nil)
}

func rememberIngestIssueUIDs(event db.RemoteEvent, known map[string]struct{}) {
	issueUID, err := payloadIssueUID(event, db.PayloadMap(event.Payload))
	if err != nil || issueUID == "" {
		return
	}
	if event.Type == "issue.created" || event.Type == "issue.snapshot" {
		known[issueUID] = struct{}{}
	}
}
