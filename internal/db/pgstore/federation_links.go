package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/db"
)

type federatedLinkRow struct {
	id      int64
	fromID  int64
	toID    int64
	fromUID string
	toUID   string
	typ     string
	author  string
}

func (row federatedLinkRow) key() db.FoldLinkKey {
	return db.FoldLinkKey{FromUID: row.fromUID, ToUID: row.toUID, Type: row.typ}
}

func federationBindingTransitionState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (*db.FederationBinding, error) {
	previous, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &previous, nil
}

func reconcileFederationBindingTransitionLinks(
	ctx context.Context,
	tx *sql.Tx,
	previous *db.FederationBinding,
	current db.FederationBinding,
) error {
	if previous != nil && previous.Enabled {
		remainingProjectIDs, err := federationBindingGroupProjectIDs(ctx, tx, *previous)
		if err != nil {
			return err
		}
		if err := reconcileFederatedLinkGroup(ctx, tx, remainingProjectIDs, 0, nil); err != nil {
			return err
		}
		stillMember := false
		for _, projectID := range remainingProjectIDs {
			if projectID == previous.ProjectID {
				stillMember = true
				break
			}
		}
		if !stillMember {
			if err := removeFederatedBoundaryLinksBetweenProjectAndGroup(
				ctx, tx, previous.ProjectID, remainingProjectIDs,
			); err != nil {
				return err
			}
		}
	}
	if !current.Enabled {
		return nil
	}
	currentProjectIDs, err := federationBindingGroupProjectIDs(ctx, tx, current)
	if err != nil {
		return err
	}
	if err := removeIncompatibleFederatedBoundaryLinks(ctx, tx, current.ProjectID, currentProjectIDs); err != nil {
		return err
	}
	return reconcileFederatedLinkGroup(ctx, tx, currentProjectIDs, 0, nil)
}

func normalizedFederationHubOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid federation hub URL %q", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	switch scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return "", fmt.Errorf("invalid federation hub URL %q: unsupported scheme %q", raw, parsed.Scheme)
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(parsed.Hostname()), port), nil
}

func federationBindingGroupProjectIDs(
	ctx context.Context,
	tx *sql.Tx,
	current db.FederationBinding,
) ([]int64, error) {
	switch current.Role {
	case db.FederationRoleHub:
	case db.FederationRoleSpoke:
	default:
		return nil, fmt.Errorf("unsupported federation group role %q", current.Role)
	}
	currentOrigin := ""
	if current.Role == db.FederationRoleSpoke {
		var err error
		currentOrigin, err = normalizedFederationHubOrigin(current.HubURL)
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx,
		federationBindingSelect+` WHERE role=$1 AND enabled=1 ORDER BY project_id ASC FOR UPDATE`,
		string(current.Role))
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var projectIDs []int64
	for rows.Next() {
		candidate, err := scanFederationBinding(rows)
		if err != nil {
			return nil, err
		}
		if current.Role == db.FederationRoleSpoke {
			candidateOrigin, err := normalizedFederationHubOrigin(candidate.HubURL)
			if err != nil {
				if candidate.ProjectID == current.ProjectID {
					return nil, err
				}
				continue
			}
			if candidateOrigin != currentOrigin {
				continue
			}
		}
		projectIDs = append(projectIDs, candidate.ProjectID)
	}
	return projectIDs, mapSQLError(rows.Err(), nil)
}

func reconcileFederatedLinkGroup(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	currentProjectID int64,
	currentIssueIDs map[string]int64,
) error {
	projection, err := federationGroupFoldProjection(ctx, tx, projectIDs)
	if err != nil {
		return err
	}
	issueIDs, err := federationGroupIssueIDs(ctx, tx, projectIDs, currentProjectID, currentIssueIDs)
	if err != nil {
		return err
	}
	existing, err := federatedLinkRows(ctx, tx, projectIDs)
	if err != nil {
		return err
	}
	desired := map[db.FoldLinkKey]federatedLinkRow{}
	keys := make([]db.FoldLinkKey, 0, len(projection.Links))
	for key, state := range projection.Links {
		if state.Present {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].FromUID != keys[j].FromUID {
			return keys[i].FromUID < keys[j].FromUID
		}
		if keys[i].ToUID != keys[j].ToUID {
			return keys[i].ToUID < keys[j].ToUID
		}
		return keys[i].Type < keys[j].Type
	})
	for _, key := range keys {
		state := projection.Links[key]
		fromID, fromOK := issueIDs[key.FromUID]
		toID, toOK := issueIDs[key.ToUID]
		if !fromOK || !toOK {
			continue
		}
		fromUID, toUID := key.FromUID, key.ToUID
		if key.Type == "related" && fromID > toID {
			fromID, toID = toID, fromID
			fromUID, toUID = toUID, fromUID
		}
		row := federatedLinkRow{
			fromID: fromID, toID: toID, fromUID: fromUID, toUID: toUID,
			typ: key.Type, author: state.Author,
		}
		desired[row.key()] = row
	}
	if err := validateFederatedParentGraph(desired); err != nil {
		return err
	}
	for key, row := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id=$1`, row.id); err != nil {
			return mapSQLError(err, nil)
		}
	}
	for key, row := range desired {
		author := nonEmptyFederationAuthor(row.author)
		if existingRow, ok := existing[key]; ok {
			if existingRow.fromID == row.fromID && existingRow.toID == row.toID && existingRow.author == author {
				continue
			}
			_, err := tx.ExecContext(ctx, `UPDATE links SET from_issue_id=$1,to_issue_id=$2,
from_issue_uid=$3,to_issue_uid=$4,type=$5,author=$6 WHERE id=$7`,
				row.fromID, row.toID, row.fromUID, row.toUID, row.typ, author, existingRow.id)
			if err != nil {
				return mapSQLError(err, linkConstraintErrors)
			}
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO links(
from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author
) VALUES($1,$2,$3,$4,$5,$6)`, row.fromID, row.toID, row.fromUID, row.toUID, row.typ, author)
		if err != nil {
			return mapSQLError(err, linkConstraintErrors)
		}
	}
	return nil
}

func federationGroupFoldProjection(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
) (db.FoldProjection, error) {
	var events []db.FoldEvent
	for _, projectID := range projectIDs {
		projectEvents, err := federationFoldEvents(ctx, tx, projectID)
		if err != nil {
			return db.FoldProjection{}, err
		}
		events = append(events, projectEvents...)
	}
	return db.FoldEvents(events), nil
}

func federationFoldEvents(ctx context.Context, tx *sql.Tx, projectID int64) ([]db.FoldEvent, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.uid,e.origin_instance_uid,p.uid,e.issue_uid,
e.related_issue_uid,e.type,e.actor,e.payload,e.hlc_physical_ms,e.hlc_counter,e.created_at
FROM events e JOIN projects p ON p.id=e.project_id WHERE e.project_id=$1 ORDER BY e.id ASC`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var output []db.FoldEvent
	for rows.Next() {
		var event db.FoldEvent
		var issueUID, relatedIssueUID sql.NullString
		var payload, createdAt string
		if err := rows.Scan(&event.UID, &event.OriginInstanceUID, &event.ProjectUID,
			&issueUID, &relatedIssueUID, &event.Type, &event.Actor, &payload,
			&event.HLCPhysicalMS, &event.HLCCounter, &createdAt); err != nil {
			return nil, mapSQLError(err, nil)
		}
		if issueUID.Valid {
			event.IssueUID = issueUID.String
		}
		if relatedIssueUID.Valid {
			event.RelatedIssueUID = relatedIssueUID.String
		}
		event.Payload = json.RawMessage(payload)
		event.CreatedAt = createdAt
		output = append(output, event)
	}
	return output, mapSQLError(rows.Err(), nil)
}

func federationGroupIssueIDs(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	currentProjectID int64,
	currentIssueIDs map[string]int64,
) (map[string]int64, error) {
	if len(projectIDs) == 0 {
		return map[string]int64{}, nil
	}
	placeholders, args := postgresIDPlaceholders(projectIDs, 1)
	//nolint:gosec // IN values use generated placeholders with separately bound integer IDs.
	query := `SELECT uid,id FROM issues WHERE project_id IN (` + placeholders + `)`
	if currentProjectID > 0 {
		query += fmt.Sprintf(` AND project_id<>$%d`, len(args)+1)
		args = append(args, currentProjectID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[string]int64{}
	for rows.Next() {
		var issueUID string
		var issueID int64
		if err := rows.Scan(&issueUID, &issueID); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[issueUID] = issueID
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	for issueUID, issueID := range currentIssueIDs {
		output[issueUID] = issueID
	}
	return output, nil
}

func federatedLinkRows(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
) (map[db.FoldLinkKey]federatedLinkRow, error) {
	if len(projectIDs) == 0 {
		return map[db.FoldLinkKey]federatedLinkRow{}, nil
	}
	fromPlaceholders, fromArgs := postgresIDPlaceholders(projectIDs, 1)
	toPlaceholders, toArgs := postgresIDPlaceholders(projectIDs, len(fromArgs)+1)
	args := append(fromArgs, toArgs...)
	//nolint:gosec // IN values use generated placeholders with separately bound integer IDs.
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.from_issue_id,l.to_issue_id,
l.from_issue_uid,l.to_issue_uid,l.type,l.author FROM links l
JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id
WHERE f.project_id IN (`+fromPlaceholders+`) AND t.project_id IN (`+toPlaceholders+`)`, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[db.FoldLinkKey]federatedLinkRow{}
	for rows.Next() {
		var row federatedLinkRow
		if err := rows.Scan(&row.id, &row.fromID, &row.toID, &row.fromUID, &row.toUID,
			&row.typ, &row.author); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[row.key()] = row
	}
	return output, mapSQLError(rows.Err(), nil)
}

func removeFederatedBoundaryLinksBetweenProjectAndGroup(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	groupProjectIDs []int64,
) error {
	if len(groupProjectIDs) == 0 {
		return nil
	}
	placeholders, groupArgs := postgresIDPlaceholders(groupProjectIDs, 2)
	args := append([]any{projectID}, groupArgs...)
	//nolint:gosec // IN values use generated placeholders with separately bound integer IDs.
	_, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id IN (
SELECT l.id FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id
WHERE (f.project_id=$1 AND t.project_id IN (`+placeholders+`))
   OR (t.project_id=$1 AND f.project_id IN (`+placeholders+`)))`, args...)
	return mapSQLError(err, nil)
}

func removeIncompatibleFederatedBoundaryLinks(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	compatibleProjectIDs []int64,
) error {
	compatible := make(map[int64]struct{}, len(compatibleProjectIDs))
	for _, id := range compatibleProjectIDs {
		compatible[id] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id,
CASE WHEN f.project_id=$1 THEN t.project_id ELSE f.project_id END AS peer_project_id
FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id
JOIN federation_bindings peer_binding ON peer_binding.project_id=CASE
WHEN f.project_id=$1 THEN t.project_id ELSE f.project_id END AND peer_binding.enabled=1
WHERE f.project_id<>t.project_id AND (f.project_id=$1 OR t.project_id=$1)`, projectID)
	if err != nil {
		return mapSQLError(err, nil)
	}
	var stale []int64
	for rows.Next() {
		var linkID, peerProjectID int64
		if err := rows.Scan(&linkID, &peerProjectID); err != nil {
			_ = rows.Close()
			return mapSQLError(err, nil)
		}
		if _, ok := compatible[peerProjectID]; !ok {
			stale = append(stale, linkID)
		}
	}
	if err := rows.Close(); err != nil {
		return mapSQLError(err, nil)
	}
	if err := rows.Err(); err != nil {
		return mapSQLError(err, nil)
	}
	for _, linkID := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id=$1`, linkID); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}

func validateFederatedParentGraph(desired map[db.FoldLinkKey]federatedLinkRow) error {
	parents := map[string]string{}
	for key := range desired {
		if key.Type != "parent" {
			continue
		}
		if existing, ok := parents[key.FromUID]; ok && existing != key.ToUID {
			return fmt.Errorf("%w: issue %s has multiple parents", db.ErrFederationIngestValidation, key.FromUID)
		}
		parents[key.FromUID] = key.ToUID
	}
	for childUID := range parents {
		current := childUID
		seen := map[string]struct{}{}
		for depth := 0; depth < db.MaxParentDepth; depth++ {
			if _, ok := seen[current]; ok {
				return fmt.Errorf("%w: %w", db.ErrFederationIngestValidation, db.ErrParentCycle)
			}
			seen[current] = struct{}{}
			parentUID, ok := parents[current]
			if !ok {
				break
			}
			current = parentUID
			if depth == db.MaxParentDepth-1 {
				return fmt.Errorf("%w: parent chain exceeds depth limit %d",
					db.ErrFederationIngestValidation, db.MaxParentDepth)
			}
		}
	}
	return nil
}

func postgresIDPlaceholders(ids []int64, start int) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		placeholders[index] = fmt.Sprintf("$%d", start+index)
		args[index] = id
	}
	return strings.Join(placeholders, ","), args
}

func nonEmptyFederationAuthor(author string) string {
	if strings.TrimSpace(author) != "" {
		return author
	}
	return "federation"
}
