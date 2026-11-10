package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"maps"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

type preparedFederationRebuildProject struct {
	id              int64
	events          []db.FoldEvent
	moveEvents      []db.FoldEvent
	projection      db.FoldProjection
	moveProjection  db.FoldProjection
	moved           db.FoldProjection
	decisions       []db.FederatedMoveDecision
	materialization federationMaterialization
}

type preparedFederationRebuild struct {
	projects  []preparedFederationRebuildProject
	moved     map[string]db.FederatedMoveDecision
	moveStamp [32]byte
}

func (d *Store) prepareFederationRebuild(ctx context.Context, projectID int64) (preparedFederationRebuild, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return preparedFederationRebuild{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result := preparedFederationRebuild{moved: map[string]db.FederatedMoveDecision{}}
	prepare := func(id int64) ([]int64, error) {
		events, err := federationFoldEvents(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if err := db.ValidateFederationEntries(event.Type, event.UID, event.Payload); err != nil {
				return nil, err
			}
		}
		projection := db.FoldEvents(events)
		decisions, destinations, err := db.PlanFederatedMovedIssues(ctx, tx, events, projection)
		if err != nil {
			return nil, err
		}
		for _, decision := range decisions {
			result.moved[decision.IssueUID] = decision
		}
		moveProjection := projection
		moveProjection.Issues = make(map[string]db.FoldIssue, len(decisions))
		for _, decision := range decisions {
			moveProjection.Issues[decision.IssueUID] = projection.Issues[decision.IssueUID]
		}
		projection.Issues = maps.Clone(projection.Issues)
		projection.Comments = maps.Clone(projection.Comments)
		projection.Labels = maps.Clone(projection.Labels)
		var moveEvents []db.FoldEvent
		for _, event := range events {
			if event.Type == "issue.moved" {
				moveEvents = append(moveEvents, event)
			}
		}
		result.projects = append(result.projects, preparedFederationRebuildProject{id: id, events: events, moveEvents: moveEvents, projection: projection, moveProjection: moveProjection, decisions: decisions})
		return destinations, nil
	}
	destinations, err := prepare(projectID)
	if err != nil {
		return preparedFederationRebuild{}, err
	}
	// Match the locked rebuild's single destination pass; it does not follow
	// moves discovered while reconciling those destinations.
	for _, id := range destinations {
		if id != projectID {
			if _, err := prepare(id); err != nil {
				return preparedFederationRebuild{}, err
			}
		}
	}
	result.moveStamp, err = federationMovedHistoryStamp(ctx, tx, result.moved)
	if err != nil {
		return preparedFederationRebuild{}, err
	}
	var maxEventID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events`).Scan(&maxEventID); err != nil {
		return preparedFederationRebuild{}, err
	}
	for i := range result.projects {
		p := &result.projects[i]
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, p.id))
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		m := federationMaterialization{binding: binding, fullHistory: true, linksAffected: true, maxEventID: maxEventID, moveIssueUIDs: map[string]struct{}{}}
		m.recordMoveIssueUIDs(p.events)
		m.eventStamp, err = federationEventStamp(ctx, tx, p.id, nil)
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		if err := m.foldLinks(ctx, tx, nil, true); err != nil {
			return preparedFederationRebuild{}, err
		}
		m.linkStamps = map[int64][32]byte{}
		for _, id := range m.linkReadProjects {
			if id == p.id {
				continue
			}
			m.linkStamps[id], err = federationEventStamp(ctx, tx, id, db.FederationLinkAffectingEventTypes())
			if err != nil {
				return preparedFederationRebuild{}, err
			}
		}
		m.rowStamp, err = federationRowStamp(ctx, tx, p.id, m.linkProjects)
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		projectUID, err := projectUIDTx(ctx, tx, p.id)
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		db.RestrictFoldProject(&p.projection, projectUID)
		p.moved = p.projection
		p.moved.Issues = maps.Clone(p.projection.Issues)
		p.moved.Comments = maps.Clone(p.projection.Comments)
		p.moved.Labels = maps.Clone(p.projection.Labels)
		p.moved.IssueMetadata = maps.Clone(p.projection.IssueMetadata)
		p.moved.ProjectMetadata = nil
		for uid := range p.projection.Issues {
			if _, ok := result.moved[uid]; ok {
				delete(p.projection.Issues, uid)
			} else {
				delete(p.moved.Issues, uid)
			}
		}
		for uid, comment := range p.projection.Comments {
			if _, ok := result.moved[comment.IssueUID]; ok {
				delete(p.projection.Comments, uid)
			} else {
				delete(p.moved.Comments, uid)
			}
		}
		for key := range p.projection.Labels {
			if _, ok := result.moved[key.IssueUID]; ok {
				delete(p.projection.Labels, key)
			} else {
				delete(p.moved.Labels, key)
			}
		}
		m.projection = p.projection
		m.cronAffected = true
		if err := m.prepareRows(ctx, tx, p.id, nil); err != nil {
			return preparedFederationRebuild{}, err
		}
		for uid := range m.issueRows {
			m.moveIssueUIDs[uid] = struct{}{}
		}
		for uid, decision := range result.moved {
			if !decision.Joined {
				continue
			}
			delete(m.staleIssues, uid)
			delete(m.issueRows, uid)
			if decision.Exists && decision.SourceID == p.id {
				for key, row := range m.commentRows {
					if row.issueID == decision.IssueID {
						delete(m.commentRows, key)
					}
				}
				for key := range m.labelRows {
					if key.issueID == decision.IssueID {
						delete(m.labelRows, key)
					}
				}
			}
			if decision.Joined && decision.Active && slices.Contains(m.linkProjects, decision.TargetID) {
				m.issueIDs[uid] = decision.IssueID
			}
		}
		p.materialization = m
	}
	for i := range result.projects {
		p := &result.projects[i]
		included := map[string]int64{}
		excluded := map[string]bool{}
		for _, other := range result.projects {
			if !slices.Contains(p.materialization.linkProjects, other.id) {
				continue
			}
			maps.Copy(included, other.materialization.issueIDs)
			for uid := range other.materialization.projection.Issues {
				if _, ok := included[uid]; !ok {
					included[uid] = 0
				}
			}
			for uid := range other.materialization.staleIssues {
				excluded[uid] = true
			}
		}
		if err := p.materialization.prepareLinksWithIssues(ctx, tx, p.id, nil, included, excluded); err != nil {
			return preparedFederationRebuild{}, err
		}
	}
	return result, nil
}

func (d *Store) applyFederationRebuild(ctx context.Context, prepared preparedFederationRebuild) error {
	parent := ctx
	ctx, resets := db.WithFederationMoveResetCapture(ctx)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stamp, err := federationMovedHistoryStamp(ctx, tx, prepared.moved)
	if err != nil {
		return err
	}
	if stamp != prepared.moveStamp {
		return errStaleFederationPreparation
	}
	for _, p := range prepared.projects {
		if err := db.LockCronProject(ctx, tx, p.id); err != nil {
			return err
		}
		if err := checkFullPullPreparation(ctx, tx, p.id, p.materialization); err != nil {
			return err
		}
		decisions, _, err := db.PlanFederatedMovedIssues(ctx, tx, p.moveEvents, p.moveProjection)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(decisions, p.decisions) {
			return errStaleFederationPreparation
		}
	}
	// The existing move routine keeps its live claim, recurrence, and import guards.
	for _, p := range prepared.projects {
		projection := p.moveProjection
		projection.Issues = maps.Clone(projection.Issues)
		if _, err := d.rehomeFederatedMovedIssues(ctx, tx, p.moveEvents, projection, nil); err != nil {
			return err
		}
	}
	validator := db.NewCronReplayValidator(tx, false)
	for _, p := range prepared.projects {
		m := p.materialization
		m.linkProjects = nil
		m.staleIssues = nil
		if err := m.apply(ctx, tx, p.id, validator); err != nil {
			return err
		}
		if len(p.moved.Issues) > 0 {
			scope := make([]string, 0, len(p.moved.Issues))
			for uid := range p.moved.Issues {
				scope = append(scope, uid)
			}
			slices.Sort(scope)
			moved := federationMaterialization{projection: p.moved, binding: m.binding}
			if err := moved.prepareRows(ctx, tx, p.id, scope); err != nil {
				return err
			}
			if err := moved.apply(ctx, tx, p.id, validator); err != nil {
				return err
			}
			maps.Copy(p.materialization.linkIDs, moved.issueIDs)
		}
	}
	var groups [][]int64
	for _, p := range prepared.projects {
		m := p.materialization
		if slices.ContainsFunc(groups, func(group []int64) bool { return slices.Equal(group, m.linkProjects) }) {
			continue
		}
		groups = append(groups, m.linkProjects)
		for _, other := range prepared.projects {
			for uid, id := range other.materialization.linkIDs {
				if _, ok := m.linkIDs[uid]; ok && id != 0 {
					m.linkIDs[uid] = id
				}
			}
		}
		if err := reconcileFederatedLinkRows(ctx, tx, m.links, m.linkIDs, m.linkRows); err != nil {
			return err
		}
	}
	for _, p := range prepared.projects {
		if err := deleteUnreferencedFederatedIssues(ctx, tx, p.materialization.staleIssues); err != nil {
			return err
		}
	}
	if err := db.RecordFederationCurrentProjectResets(ctx, tx, db.FederationPulledEventUIDs(ctx), func(project db.Project) (int64, error) {
		event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: db.ProjectScopeResetEventType, Actor: "system", Payload: "{}", SkipRelay: true})
		return event.ID, err
	}); err != nil {
		return err
	}
	if err := db.RecordMCPEventsRematch(ctx, tx, db.FederationPulledEventUIDs(ctx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.CommitFederationMoveResets(parent, resets.Resets)
	return nil
}

func federationMovedHistoryStamp(ctx context.Context, tx *sql.Tx, moved map[string]db.FederatedMoveDecision) ([32]byte, error) {
	uids := make([]string, 0, len(moved))
	for uid := range moved {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	h := sha256.New()
	for start := 0; start < len(uids); start += federationIngestIssueScopeLimit {
		end := min(start+federationIngestIssueScopeLimit, len(uids))
		args := make([]any, 0, end-start)
		for _, uid := range uids[start:end] {
			args = append(args, uid)
		}
		// The expression matches the existing resolved issue identity index.
		//nolint:gosec // Only a fixed identity expression and generated placeholders enter the query.
		query := `SELECT id,content_hash FROM events e WHERE ` + db.FederationReadIssueUIDExpr(false) + ` IN (?` + strings.Repeat(",?", len(args)-1) + `) ORDER BY id`
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return [32]byte{}, err
		}
		for rows.Next() {
			var id int64
			var hash string
			if err := rows.Scan(&id, &hash); err != nil {
				_ = rows.Close()
				return [32]byte{}, err
			}
			var encoded [8]byte
			binary.BigEndian.PutUint64(encoded[:], uint64(id)) //nolint:gosec // Preserve all bits of the SQLite row ID.
			_, _ = h.Write(encoded[:])
			_, _ = h.Write([]byte(hash))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return [32]byte{}, err
		}
		if err := rows.Close(); err != nil {
			return [32]byte{}, err
		}
	}
	return [32]byte(h.Sum(nil)), nil
}
