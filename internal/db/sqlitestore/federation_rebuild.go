package sqlitestore

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"go.kenn.io/kata/internal/db"
)

type preparedFederationRebuildProject struct {
	id              int64
	uid             string
	events          []db.FoldEvent
	moveEvents      map[string]db.FoldEvent
	projection      db.FoldProjection
	moveProjection  db.FoldProjection
	moved           db.FoldProjection
	decisions       []db.FederatedMoveDecision
	linkEvents      []db.FoldEvent
	materialization federationMaterialization
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
	moveOwners := map[string]string{}
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
		moveEvents := db.IndexFederatedMoveEvents(events)
		decisions, destinations, err := db.PlanFederatedMovedIssues(ctx, tx, moveEvents, projection)
		if err != nil {
			return nil, err
		}
		moveProjection := db.FoldProjection{Issues: make(map[string]db.FoldIssue, len(decisions))}
		for _, decision := range decisions {
			result.moved[decision.IssueUID] = decision
			moveOwners[decision.IssueUID] = projection.Issues[decision.IssueUID].ProjectUID
			moveProjection.Issues[decision.IssueUID] = projection.Issues[decision.IssueUID]
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
		m := federationMaterialization{binding: binding, linksUnderWriter: true}
		if err := m.prepareFullHistory(ctx, tx, p.id, p.events, true); err != nil {
			return preparedFederationRebuild{}, err
		}
		if err := m.foldLinks(ctx, tx, nil, true, &p.linkEvents); err != nil {
			return preparedFederationRebuild{}, err
		}
		if err := m.stampFullHistoryLinks(ctx, tx, p.id); err != nil {
			return preparedFederationRebuild{}, err
		}
		projectUID, err := projectUIDTx(ctx, tx, p.id)
		if err != nil {
			return preparedFederationRebuild{}, err
		}
		p.uid = projectUID
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
		m := &p.materialization
		events := p.linkEvents
		if slices.ContainsFunc(p.linkEvents, m.relayOrigins.relaySourced) {
			owners, err := relayLinkEndpointProjects(ctx, tx, m.linkProjects)
			if err != nil {
				return preparedFederationRebuild{}, err
			}
			for _, other := range result.projects {
				for uid := range other.materialization.staleIssues {
					delete(owners, uid)
				}
				if !slices.Contains(m.linkProjects, other.id) {
					continue
				}
				for uid := range other.projection.Issues {
					owners[uid] = other.uid
				}
				for uid := range other.moved.Issues {
					owners[uid] = other.uid
				}
			}
			maps.Copy(owners, moveOwners)
			events, err = filterRelayLinkEvents(events, m.relayOrigins, owners)
			if err != nil {
				return preparedFederationRebuild{}, err
			}
		}
		m.links = db.FoldEvents(events)
		if _, err := federationGroupIssueIDs(ctx, tx, m.linkProjects, 0, nil, nil, m.rowHasher); err != nil {
			return preparedFederationRebuild{}, err
		}
		m.rowStamp = m.rowHasher.sum()
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
		}
		// Earlier rehomes can change the source of a later decision.
		changed := map[string]bool{}
		for _, p := range prepared.projects {
			decisions, _, err := db.PlanFederatedMovedIssues(ctx, tx, p.moveEvents, p.moveProjection)
			if err != nil {
				return err
			}
			for i, decision := range decisions {
				if !changed[decision.IssueUID] && decision != p.decisions[i] {
					return errStaleFederationPreparation
				}
			}
			if err := d.rehomeFederatedMovedIssues(ctx, tx, p.moveEvents, p.moveProjection, decisions, nil); err != nil {
				return err
			}
			for _, decision := range decisions {
				if isFederationRehome(decision) {
					changed[decision.IssueUID] = true
				}
			}
		}

		validator := db.NewCronReplayValidator(tx, false)
		for i := range prepared.projects {
			p := prepared.projects[(i+1)%len(prepared.projects)]
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
			}
		}
		var groups [][]int64
		for _, p := range prepared.projects {
			m := p.materialization
			if slices.ContainsFunc(groups, func(group []int64) bool { return slices.Equal(group, m.linkProjects) }) {
				continue
			}
			groups = append(groups, m.linkProjects)
			ids, err := federationGroupIssueIDs(ctx, tx, m.linkProjects, 0, nil, nil, nil)
			if err != nil {
				return err
			}
			for _, other := range prepared.projects {
				for uid := range other.materialization.staleIssues {
					delete(ids, uid)
				}
			}
			rows, err := federatedLinkRows(ctx, tx, m.linkProjects, nil)
			if err != nil {
				return err
			}
			if err := reconcileFederatedLinkRows(ctx, tx, m.links, ids, rows); err != nil {
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
