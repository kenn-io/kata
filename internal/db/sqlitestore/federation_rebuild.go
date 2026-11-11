package sqlitestore

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"go.kenn.io/kata/internal/db"
)

type preparedFederationRebuildProject struct {
	id               int64
	events           []db.FoldEvent
	projection       db.FoldProjection
	moveProjection   db.FoldProjection
	moved            db.FoldProjection
	decisions        []db.FederatedMoveDecision
	materialization  federationMaterialization
	linksUnderWriter bool
}

type preparedFederationRebuild struct {
	projects  []preparedFederationRebuildProject
	moved     map[string]db.FederatedMoveDecision
	moveStamp [32]byte
	moveUIDs  []string
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
		moveProjection := db.FoldProjection{Issues: make(map[string]db.FoldIssue, len(decisions))}
		for _, decision := range decisions {
			result.moved[decision.IssueUID] = decision
			moveProjection.Issues[decision.IssueUID] = projection.Issues[decision.IssueUID]
		}
		result.projects = append(result.projects, preparedFederationRebuildProject{id: id, events: events, projection: projection, moveProjection: moveProjection, decisions: decisions})
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
	result.moveUIDs = slices.AppendSeq([]string{}, maps.Keys(result.moved))
	result.moveStamp, err = federationEventStamp(ctx, tx, 0, nil, result.moveUIDs)
	if err != nil {
		return preparedFederationRebuild{}, err
	}
	for i := range result.projects {
		p := &result.projects[i]
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, p.id))
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		m := federationMaterialization{binding: binding}
		if err := m.prepareFullHistory(ctx, tx, p.events, true); err != nil {
			return preparedFederationRebuild{}, err
		}
		if err := m.foldLinks(ctx, tx, nil, true); err != nil {
			return preparedFederationRebuild{}, err
		}
		if err := m.stampFullHistoryRows(ctx, tx, p.id); err != nil {
			return preparedFederationRebuild{}, err
		}
		projectUID, err := projectUIDTx(ctx, tx, p.id)
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		p.moved = p.projection
		p.moved.Issues = maps.Clone(p.projection.Issues)
		p.moved.Comments = maps.Clone(p.projection.Comments)
		p.moved.Labels = maps.Clone(p.projection.Labels)
		db.RestrictFoldProjectWhere(&p.projection, projectUID, func(uid string) bool {
			decision, ok := result.moved[uid]
			return !ok || (decision.Joined && (decision.Active || decision.Exists) && !isFederationRehome(decision))
		})
		db.RestrictFoldProjectWhere(&p.moved, projectUID, func(uid string) bool {
			decision, ok := result.moved[uid]
			return ok && isFederationRehome(decision)
		})
		m.projection = p.projection
		m.cronAffected = true
		if err := m.prepareRows(ctx, tx, p.id, nil); err != nil {
			return preparedFederationRebuild{}, err
		}
		for uid := range m.issueRows {
			m.moveIssueUIDs[uid] = struct{}{}
		}
		movedIssueIDs := map[int64]struct{}{}
		for uid, decision := range result.moved {
			if !isFederationRehome(decision) {
				continue
			}
			delete(m.staleIssues, uid)
			delete(m.issueRows, uid)
			if decision.SourceID == p.id {
				movedIssueIDs[decision.IssueID] = struct{}{}
				m.issueIDs[uid] = decision.IssueID
			}
		}
		for key, row := range m.commentRows {
			if _, ok := movedIssueIDs[row.issueID]; ok {
				delete(m.commentRows, key)
			}
		}
		for key := range m.labelRows {
			if _, ok := movedIssueIDs[key.issueID]; ok {
				delete(m.labelRows, key)
			}
		}
		p.materialization = m
	}
	for i := range result.projects {
		p := &result.projects[i]
		// A former source outside this group can hold links until rehome.
		for _, decision := range result.moved {
			if isFederationRehome(decision) && slices.Contains(p.materialization.linkProjects, decision.TargetID) && !slices.Contains(p.materialization.linkProjects, decision.SourceID) {
				p.linksUnderWriter = true
				break
			}
		}
		if p.linksUnderWriter {
			continue
		}
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
		if err := p.materialization.prepareLinks(ctx, tx, p.id, nil, included, excluded); err != nil {
			return preparedFederationRebuild{}, err
		}
	}
	return result, nil
}

func (d *Store) applyFederationRebuild(ctx context.Context, prepared preparedFederationRebuild) error {
	return d.withFederationMaterialization(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stamp, err := federationEventStamp(ctx, tx, 0, nil, prepared.moveUIDs)
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
			decisions, _, err := db.PlanFederatedMovedIssues(ctx, tx, p.events, p.moveProjection)
			if err != nil {
				return err
			}
			if !slices.Equal(decisions, p.decisions) {
				return errStaleFederationPreparation
			}
		}
		// The existing move routine keeps its live claim, recurrence, and import guards.
		for _, p := range prepared.projects {
			if _, err := d.rehomeFederatedMovedIssues(ctx, tx, p.events, p.moveProjection, nil); err != nil {
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
				scope := slices.Sorted(maps.Keys(p.moved.Issues))
				moved := federationMaterialization{projection: p.moved, binding: m.binding}
				if err := moved.prepareRows(ctx, tx, p.id, scope); err != nil {
					return err
				}
				if err := moved.apply(ctx, tx, p.id, validator); err != nil {
					return err
				}
				maps.Copy(p.materialization.issueIDs, moved.issueIDs)
				if p.materialization.linkIDs != nil {
					maps.Copy(p.materialization.linkIDs, moved.issueIDs)
				}
			}
		}
		var groups [][]int64
		for _, p := range prepared.projects {
			m := p.materialization
			if slices.ContainsFunc(groups, func(group []int64) bool { return slices.Equal(group, m.linkProjects) }) {
				continue
			}
			groups = append(groups, m.linkProjects)
			if p.linksUnderWriter {
				included := map[string]int64{}
				excluded := map[string]bool{}
				for _, other := range prepared.projects {
					if slices.Contains(m.linkProjects, other.id) {
						maps.Copy(included, other.materialization.issueIDs)
						for uid := range other.materialization.staleIssues {
							excluded[uid] = true
						}
					}
				}
				if err := m.prepareLinks(ctx, tx, p.id, nil, included, excluded); err != nil {
					return err
				}
			} else {
				for _, other := range prepared.projects {
					for uid, id := range other.materialization.linkIDs {
						if _, ok := m.linkIDs[uid]; ok && id != 0 {
							m.linkIDs[uid] = id
						}
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
		return nil
	})
}

func isFederationRehome(decision db.FederatedMoveDecision) bool {
	return decision.Joined && decision.Active && decision.Exists && decision.SourceID != decision.TargetID
}
