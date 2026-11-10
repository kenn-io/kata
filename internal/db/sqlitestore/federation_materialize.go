package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// Scoped preparation uses an optimistic transaction. Full-history pulls fold
// in a read-only snapshot and compare the event stamp under the writer lock.
// Both paths fall back to preparation under the lock after retry attempts.
type federationMaterialization struct {
	fullReplay       bool
	fullHistory      bool
	linksAffected    bool
	eventStamp       [32]byte
	rowStamp         [32]byte
	shortIDChecks    []federatedShortIDCheck
	linkStamps       map[int64][32]byte
	linkReadProjects []int64
	maxEventID       int64
	moveIssueUIDs    map[string]struct{}
	relayOrigins     relayOriginClassifier
	projection       db.FoldProjection
	cronAffected     bool
	status           []federatedStatusIntentUpdate
	binding          db.FederationBinding
	linkProjects     []int64
	links            db.FoldProjection
	issueRows        map[string]federatedIssueRow
	issueIDs         map[string]int64
	commentRows      map[string]federatedCommentRow
	labelRows        map[federatedLabelKey]struct{}
	staleIssues      map[string]int64
	linkIDs          map[string]int64
	linkRows         map[db.FoldLinkKey]federatedLinkRow
}

// Large issue sets keep the established complete queries. This bounds SQL
// placeholders while preserving support for pushes larger than a poll page.
const federationIngestIssueScopeLimit = 500

func prepareFederationMaterializationTx(ctx context.Context, tx *sql.Tx, projectID int64, prepared []preparedFederationIngestEvent) (federationMaterialization, error) {
	var scope []string
	for _, input := range prepared {
		if input.Duplicate {
			continue
		}
		ev := input.Event
		// Cron rows replay their complete history separately, so cron events do
		// not widen the issue scope.
		if db.EventRequiredFeatures(ev.Type) == db.CronEventFeature {
			continue
		}
		uid := db.FederationEventIssueUID(ev.IssueUID, string(ev.Payload))
		// Project events, moves and unidentified events can affect the entire
		// projection. Ordinary issue batches use the same scope as pulled pages.
		if uid == "" || strings.HasPrefix(ev.Type, "project.") || ev.Type == "issue.moved" {
			scope = nil
			break
		}
		if !slices.Contains(scope, uid) {
			scope = append(scope, uid)
		}
		if len(scope) > federationIngestIssueScopeLimit {
			scope = nil
			break
		}
	}
	return foldFederationMaterialization(ctx, tx, projectID, prepared, false, scope, false)
}

// alreadyStored selects pull preparation: the page is in the log already.
// Cron-only pages fold cron history. Other pages reconcile the supplied issue
// scope (nil is full); a scoped page with cron events adds complete cron history.
func foldFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, prepared []preparedFederationIngestEvent, alreadyStored bool, issueScope []string, deferRows bool) (federationMaterialization, error) {
	var fresh []db.FoldEvent
	var accepted []string
	linksAffected := false
	cronOnly := true
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
		cronOnly = cronOnly && db.EventRequiredFeatures(ev.Type) == db.CronEventFeature
	}
	if len(fresh) == 0 {
		return federationMaterialization{}, nil
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return federationMaterialization{}, err
	}
	if cronOnly {
		if alreadyStored {
			return prepareCronPullMaterialization(ctx, tx, projectID, binding, prepared)
		}
		return prepareCronFederationMaterialization(ctx, tx, projectID, binding, fresh, alreadyStored)
	}
	events, err := federationFoldEventsForIssues(ctx, tx, projectID, issueScope)
	if err != nil {
		return federationMaterialization{}, err
	}
	if len(issueScope) > 0 && slices.ContainsFunc(fresh, func(event db.FoldEvent) bool {
		return db.EventRequiredFeatures(event.Type) == db.CronEventFeature
	}) {
		// Scoped history can include observations referencing a touched issue.
		// Folding them twice is harmless but wasted work, so replace them with
		// the complete project cron history.
		events = slices.DeleteFunc(events, func(event db.FoldEvent) bool {
			return db.EventRequiredFeatures(event.Type) == db.CronEventFeature
		})
		cronEvents, err := federationCronFoldEvents(ctx, tx, projectID)
		if err != nil {
			return federationMaterialization{}, err
		}
		events = append(events, cronEvents...)
	}
	if !alreadyStored {
		events = append(events, fresh...)
	}
	for _, event := range events {
		if event.Type == "issue.moved" {
			return federationMaterialization{fullReplay: true}, nil
		}
	}
	for _, event := range events {
		if err := db.ValidateFederationEntries(event.Type, event.UID, event.Payload); err != nil {
			return federationMaterialization{}, err
		}
	}
	m := federationMaterialization{binding: binding, projection: db.FoldEvents(events)}
	if deferRows && len(issueScope) == 0 {
		m.fullHistory = true
		m.moveIssueUIDs = map[string]struct{}{}
		m.recordMoveIssueUIDs(events)
		m.linksAffected = linksAffected
		m.eventStamp, err = federationEventStamp(ctx, tx, projectID, nil)
		if err != nil {
			return federationMaterialization{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events`).Scan(&m.maxEventID); err != nil {
			return federationMaterialization{}, err
		}
	}
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
		if err := m.foldLinks(ctx, tx, fresh, alreadyStored); err != nil {
			return federationMaterialization{}, err
		}
	}
	if m.fullHistory {
		if linksAffected {
			m.linkStamps = make(map[int64][32]byte)
			for _, id := range m.linkReadProjects {
				if id == projectID {
					continue
				}
				stamp, err := federationEventStamp(ctx, tx, id, db.FederationLinkAffectingEventTypes())
				if err != nil {
					return federationMaterialization{}, err
				}
				m.linkStamps[id] = stamp
			}
		}
		m.rowStamp, err = federationRowStamp(ctx, tx, projectID, m.linkProjects)
		if err != nil {
			return federationMaterialization{}, err
		}
	}
	if err := m.prepareRows(ctx, tx, projectID, issueScope); err != nil {
		return federationMaterialization{}, err
	}
	if m.fullHistory {
		for uid := range m.issueRows {
			m.moveIssueUIDs[uid] = struct{}{}
		}
	}
	if m.linkProjects != nil {
		if err := m.prepareLinks(ctx, tx, projectID, issueScope); err != nil {
			return federationMaterialization{}, err
		}
	}
	return m, nil
}

func (m *federationMaterialization) foldLinks(ctx context.Context, tx *sql.Tx, fresh []db.FoldEvent, alreadyStored bool) error {
	var err error
	m.linkProjects, err = federationBindingGroupProjectIDs(ctx, tx, m.binding)
	if err != nil {
		return err
	}
	var events []db.FoldEvent
	for _, id := range m.linkProjects {
		history, err := federationFoldEventsOfTypes(ctx, tx, id, db.FederationLinkAffectingEventTypes())
		if err != nil {
			return err
		}
		events = append(events, history...)
	}
	if m.fullHistory {
		m.recordMoveIssueUIDs(events)
		m.linkReadProjects = slices.Clone(m.linkProjects)
		seen := map[string]bool{}
		for _, event := range events {
			if seen[event.ProjectUID] {
				continue
			}
			seen[event.ProjectUID] = true
			var id int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=?`, event.ProjectUID).Scan(&id); err != nil {
				return err
			}
			if !slices.Contains(m.linkReadProjects, id) {
				m.linkReadProjects = append(m.linkReadProjects, id)
			}
		}
		slices.Sort(m.linkReadProjects)
	}
	if !alreadyStored {
		for _, event := range fresh {
			if db.FederationEventAffectsLinks(event.Type) {
				events = append(events, event)
			}
		}
	}
	events, m.relayOrigins, err = filterRelayCrossProjectLinkEvents(ctx, tx, m.linkProjects, events)
	if err != nil {
		return err
	}
	m.links = db.FoldEvents(events)
	return nil
}

func (m *federationMaterialization) recordMoveIssueUIDs(events []db.FoldEvent) {
	for _, event := range events {
		uid := event.IssueUID
		if uid == "" && strings.HasPrefix(event.Type, "issue.") {
			uid = db.FederationEventIssueUID(nil, string(event.Payload))
		}
		if uid != "" {
			m.moveIssueUIDs[uid] = struct{}{}
		}
	}
}

// federationRowStamp reads the persisted inputs used by row and link preparation.
func federationRowStamp(ctx context.Context, tx *sql.Tx, projectID int64, group []int64) ([32]byte, error) {
	type stampQuery struct {
		query string
		args  []any
	}
	queries := []stampQuery{
		{`SELECT json_array(uid,id,short_id,title,body,status,closed_reason,owner,CAST(assignment_expires_on AS TEXT),priority,author,CAST(created_at AS TEXT),CAST(updated_at AS TEXT),CAST(closed_at AS TEXT),CAST(deleted_at AS TEXT),metadata) FROM issues WHERE project_id=? ORDER BY id`, []any{projectID}},
		{`SELECT json_array(c.uid,c.id,c.issue_id,c.author,CAST(c.created_at AS TEXT),c.body,c.teammate) FROM issues i JOIN comments c ON c.issue_id=i.id WHERE i.project_id=? ORDER BY c.id`, []any{projectID}},
		{`SELECT json_array(l.issue_id,l.label) FROM issues i JOIN issue_labels l ON l.issue_id=i.id WHERE i.project_id=? ORDER BY l.issue_id,l.label`, []any{projectID}},
	}
	if len(group) > 0 {
		placeholders, args := projectIDPlaceholders(group)
		//nolint:gosec // The IN clauses contain generated placeholders; IDs are bound.
		queries = append(queries, stampQuery{`SELECT json_array(uid,id,project_id) FROM issues WHERE project_id IN (` + placeholders + `) ORDER BY project_id,id`, args})
		//nolint:gosec // The IN clauses contain generated placeholders; IDs are bound.
		queries = append(queries, stampQuery{`SELECT json_array(l.id,l.from_issue_id,l.to_issue_id,l.from_issue_uid,l.to_issue_uid,l.type,l.author,CAST(l.created_at AS TEXT)) FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id IN (` + placeholders + `) AND t.project_id IN (` + placeholders + `) ORDER BY l.id`, append(slices.Clone(args), args...)})
	}
	h := sha256.New()
	for _, q := range queries {
		rows, err := tx.QueryContext(ctx, q.query, q.args...)
		if err != nil {
			return [32]byte{}, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				return [32]byte{}, err
			}
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value))) //nolint:gosec // A string length is nonnegative.
			_, _ = h.Write(length[:])
			_, _ = h.Write([]byte(value))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return [32]byte{}, err
		}
		if err := rows.Close(); err != nil {
			return [32]byte{}, err
		}
		_, _ = h.Write([]byte{0})
	}
	var stamp [32]byte
	copy(stamp[:], h.Sum(nil))
	return stamp, nil
}

func federationEventStamp(ctx context.Context, tx *sql.Tx, projectID int64, eventTypes []string) ([32]byte, error) {
	query := `SELECT id, content_hash FROM events WHERE project_id=?`
	args := []any{projectID}
	if len(eventTypes) > 0 {
		//nolint:gosec // Only generated placeholders enter the query; event types are bound.
		query += ` AND type IN (?` + strings.Repeat(",?", len(eventTypes)-1) + `)`
		for _, eventType := range eventTypes {
			args = append(args, eventType)
		}
	}
	query += ` ORDER BY id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return [32]byte{}, fmt.Errorf("list federation event stamps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	h := sha256.New()
	for rows.Next() {
		var id int64
		var contentHash string
		if err := rows.Scan(&id, &contentHash); err != nil {
			return [32]byte{}, fmt.Errorf("scan federation event stamp: %w", err)
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(id)) //nolint:gosec // Preserve every bit of the SQLite row ID.
		_, _ = h.Write(encoded[:])
		_, _ = h.Write([]byte(contentHash))
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	return [32]byte(h.Sum(nil)), nil
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

type federatedShortIDCheck struct {
	uid, preferred, resolved string
	row                      federatedIssueRow
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
		if m.fullHistory && issue.ShortID != "" && issue.ShortID != row.shortID {
			m.shortIDChecks = append(m.shortIDChecks, federatedShortIDCheck{issue.UID, issue.ShortID, shortID, row})
		}
		values, err := federatedIssueValues(issue, shortID, m.projection.IssueMetadata[issue.UID])
		if err != nil {
			return err
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
	return m.prepareLinksWithIssues(ctx, tx, projectID, issueScope, nil, nil)
}

func (m *federationMaterialization) prepareLinksWithIssues(ctx context.Context, tx *sql.Tx, projectID int64, issueScope []string, included map[string]int64, excluded map[string]bool) error {
	ids, err := federationGroupIssueIDs(ctx, tx, m.linkProjects, projectID, m.issueIDs, issueScope)
	if err != nil {
		return err
	}
	for uid := range excluded {
		delete(ids, uid)
	}
	maps.Copy(ids, included)
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
