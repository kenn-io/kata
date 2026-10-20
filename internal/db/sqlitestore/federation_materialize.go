package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// A deferred SQLite transaction keeps this history-sized preparation out of
// the writer interval. A concurrent commit makes the first write fail with
// BUSY_SNAPSHOT; the caller retries validation and preparation together, and
// takes the project lock before preparing once optimistic attempts run out.
// Request fences still run first and may themselves acquire the writer lock.
type federationMaterialization struct {
	projection   db.FoldProjection
	cronAffected bool
	status       []federatedStatusIntentUpdate
	binding      db.FederationBinding
	linkProjects []int64
	links        db.FoldProjection
	issueRows    map[string]federatedIssueRow
	issueIDs     map[string]int64
	commentRows  map[string]federatedCommentRow
	labelRows    map[federatedLabelKey]struct{}
	staleIssues  map[string]int64
	linkIDs      map[string]int64
	linkRows     map[db.FoldLinkKey]federatedLinkRow
}

func prepareFederationMaterializationTx(ctx context.Context, tx *sql.Tx, projectID int64, prepared []preparedFederationIngestEvent) (federationMaterialization, error) {
	return prepareFederationMaterialization(ctx, tx, projectID, prepared, false, nil)
}

// alreadyStored selects pull preparation: the page is in the log already, and
// only the supplied issue histories and rows need reconciliation (nil is full).
func prepareFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, prepared []preparedFederationIngestEvent, alreadyStored bool, issueScope []string) (federationMaterialization, error) {
	var fresh []db.FoldEvent
	var accepted []string
	linksAffected := false
	for _, in := range prepared {
		if in.Duplicate {
			continue
		}
		ev := in.Event
		event := db.FoldEvent{UID: ev.EventUID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID,
			Type: ev.Type, Actor: ev.Actor, Payload: ev.Payload, HLCPhysicalMS: ev.HLCPhysicalMS, HLCCounter: ev.HLCCounter,
			CreatedAt: ev.CreatedAt.UTC().Format(sqliteTimeFormat)}
		if ev.IssueUID != nil {
			event.IssueUID = *ev.IssueUID
		}
		if ev.RelatedIssueUID != nil {
			event.RelatedIssueUID = *ev.RelatedIssueUID
		}
		fresh = append(fresh, event)
		accepted = append(accepted, event.UID)
		linksAffected = linksAffected || db.FederationEventAffectsLinks(ev.Type)
	}
	if len(fresh) == 0 {
		return federationMaterialization{}, nil
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return federationMaterialization{}, err
	}
	events, err := federationFoldEventsForIssues(ctx, tx, projectID, issueScope)
	if err != nil {
		return federationMaterialization{}, err
	}
	if !alreadyStored {
		events = append(events, fresh...)
	}
	for _, event := range events {
		if err := db.ValidateFederationEntries(event.Type, event.UID, event.Payload); err != nil {
			return federationMaterialization{}, err
		}
	}
	m := federationMaterialization{binding: binding, projection: db.FoldEvents(events)}
	if len(issueScope) > 0 {
		// Historical project events can affect issue authors, but an issue-only
		// page must not reconcile unrelated project metadata.
		m.projection.ProjectMetadata = nil
	}
	m.cronAffected = db.CronMaterializationNeeded(events, accepted)
	// Provider status intent is enqueued by hub ingest, not spoke pulls.
	if !alreadyStored {
		m.status, err = prepareFederatedStatusIntentTx(ctx, tx, projectID, events, accepted, m.projection)
		if err != nil {
			return federationMaterialization{}, err
		}
	}
	if linksAffected {
		m.linkProjects, err = federationBindingGroupProjectIDs(ctx, tx, binding)
		if err != nil {
			return federationMaterialization{}, err
		}
		var linkEvents []db.FoldEvent
		for _, id := range m.linkProjects {
			history, err := federationFoldEventsOfTypes(ctx, tx, id, db.FederationLinkAffectingEventTypes())
			if err != nil {
				return federationMaterialization{}, err
			}
			linkEvents = append(linkEvents, history...)
		}
		if !alreadyStored {
			for _, event := range fresh {
				if db.FederationEventAffectsLinks(event.Type) {
					linkEvents = append(linkEvents, event)
				}
			}
		}
		// The incremental fold reads the same group link history as a full
		// materialization, so it applies the same relay link boundary.
		linkEvents, err = filterRelayCrossProjectLinkEvents(ctx, tx, m.linkProjects, linkEvents)
		if err != nil {
			return federationMaterialization{}, err
		}
		m.links = db.FoldEvents(linkEvents)
	}
	if err := m.prepareRows(ctx, tx, projectID, issueScope); err != nil {
		return federationMaterialization{}, err
	}
	if m.linkProjects != nil {
		if err := m.prepareLinks(ctx, tx, projectID, issueScope); err != nil {
			return federationMaterialization{}, err
		}
	}
	return m, nil
}

func (m *federationMaterialization) apply(ctx context.Context, tx *sql.Tx, projectID int64, validator *db.CronReplayValidator) error {
	if m.cronAffected {
		if err := db.MaterializeCronDefinitions(ctx, tx, projectID, m.binding.HubProjectUID, m.projection, validator); err != nil {
			return err
		}
	}
	changedIDs, err := reconcileFederatedIssuesFromRows(ctx, tx, projectID, m.projection, m.issueRows)
	if err != nil {
		return err
	}
	for uid, id := range changedIDs {
		m.issueIDs[uid] = id
		if m.linkIDs != nil {
			m.linkIDs[uid] = id
		}
	}
	if err := applyFederatedStatusIntentTx(ctx, tx, m.status); err != nil {
		return err
	}
	if err := reconcileFederatedCommentsFromRows(ctx, tx, m.issueIDs, m.projection, m.commentRows); err != nil {
		return err
	}
	if err := reconcileFederatedLabelsFromRows(ctx, tx, m.issueIDs, m.projection, m.labelRows); err != nil {
		return err
	}
	if m.linkProjects != nil {
		if err := reconcileFederatedLinkRows(ctx, tx, m.links, m.linkIDs, m.linkRows); err != nil {
			return err
		}
	}
	if err := deleteUnreferencedFederatedIssues(ctx, tx, m.staleIssues); err != nil {
		return err
	}
	return updateFederatedProjectMetadataTx(ctx, tx, projectID, m.projection.ProjectMetadata[m.binding.HubProjectUID])
}

// prepareRows compares against persisted values, rather than a previous fold:
// local writes and imports can leave values that a full rebuild would repair.
func (m *federationMaterialization) prepareRows(ctx context.Context, tx *sql.Tx, projectID int64, issueScope []string) error {
	existing, err := federatedIssueRowsByUID(ctx, tx, projectID, issueScope...)
	if err != nil {
		return err
	}
	m.issueRows = existing
	m.issueIDs = make(map[string]int64, len(existing))
	m.staleIssues = map[string]int64{}
	for uid, row := range existing {
		if _, ok := m.projection.Issues[uid]; ok {
			m.issueIDs[uid] = row.id
		} else {
			m.staleIssues[uid] = row.id
		}
	}
	for _, issue := range m.projection.SortedIssues() {
		row, ok := existing[issue.UID]
		if !ok {
			continue
		}
		shortID, err := resolveFederatedIssueShortID(ctx, tx, projectID, issue.UID, issue.ShortID, &row)
		if err != nil {
			return err
		}
		metadata := string(m.projection.IssueMetadata[issue.UID])
		if metadata == "" {
			metadata = "{}"
		}
		updatedAt := issue.UpdatedAt
		if updatedAt == "" {
			updatedAt = issue.CreatedAt
		}
		values := []any{shortID, issue.Title, issue.Body, nonEmptyStatus(issue.Status), issue.ClosedReason, issue.Owner,
			issue.AssignmentExpiresOn, issue.Priority, nonEmptyAuthor(issue.Author), nonEmptyTime(issue.CreatedAt),
			nonEmptyTime(updatedAt), optionalStringValue(issue.ClosedAt), optionalStringValue(issue.DeletedAt), metadata}
		for i, value := range values {
			values[i], err = driver.DefaultParameterConverter.ConvertValue(value)
			if err != nil {
				return err
			}
		}
		if slices.Equal(values, row.values) {
			delete(m.projection.Issues, issue.UID)
		}
	}
	comments, err := federatedCommentRowsByUID(ctx, tx, projectID, issueScope...)
	if err != nil {
		return err
	}
	m.commentRows = comments
	for uid, comment := range m.projection.Comments {
		row, ok := comments[uid]
		if !ok {
			continue
		}
		issueID, exists := m.issueIDs[comment.IssueUID]
		if exists && row.issueID == issueID && row.author == nonEmptyAuthor(comment.Author) && row.body == comment.Body &&
			row.teammate == comment.Teammate && row.createdAt == nonEmptyTime(comment.CreatedAt) {
			delete(m.projection.Comments, uid)
			delete(comments, uid)
		}
	}
	labels, err := federatedLabelKeys(ctx, tx, projectID, issueScope...)
	if err != nil {
		return err
	}
	m.labelRows = labels
	for key, state := range m.projection.Labels {
		if !state.Present {
			delete(m.projection.Labels, key)
			continue
		}
		id, ok := m.issueIDs[key.IssueUID]
		if !ok {
			continue
		}
		stored := federatedLabelKey{issueID: id, label: key.Label}
		if _, ok := labels[stored]; ok {
			delete(labels, stored)
			delete(m.projection.Labels, key)
		}
	}
	return nil
}

func canonicalFederatedLinkKey(key db.FoldLinkKey) db.FoldLinkKey {
	if key.Type == "related" && key.FromUID > key.ToUID {
		key.FromUID, key.ToUID = key.ToUID, key.FromUID
	}
	return key
}

func (m *federationMaterialization) prepareLinks(ctx context.Context, tx *sql.Tx, projectID int64, issueScope []string) error {
	ids, err := federationGroupIssueIDs(ctx, tx, m.linkProjects, projectID, m.issueIDs, issueScope)
	if err != nil {
		return err
	}
	// New endpoints have no row ID yet. Presence is sufficient for graph
	// validation; application substitutes their IDs after sorted issue inserts.
	for uid := range m.projection.Issues {
		if _, ok := ids[uid]; !ok {
			ids[uid] = 0
		}
	}
	m.linkIDs = ids
	existing, err := federatedLinkRows(ctx, tx, m.linkProjects)
	if err != nil {
		return err
	}
	m.linkRows = existing
	byUID := make(map[db.FoldLinkKey]db.FoldLinkKey, len(existing))
	for key := range existing {
		byUID[canonicalFederatedLinkKey(key)] = key
	}
	graph := map[db.FoldLinkKey]federatedLinkRow{}
	for key, state := range m.links.Links {
		_, from := ids[key.FromUID]
		_, to := ids[key.ToUID]
		if !state.Present || !from || !to {
			delete(m.links.Links, key)
			continue
		}
		graph[key] = federatedLinkRow{}
		oldKey, ok := byUID[canonicalFederatedLinkKey(key)]
		if !ok {
			continue
		}
		row := existing[oldKey]
		if row.author == nonEmptyAuthor(state.Author) && (state.CreatedAt == "" || row.createdAt == state.CreatedAt) {
			delete(existing, oldKey)
			delete(m.links.Links, key)
		}
	}
	return validateFederatedParentGraph(graph)
}
