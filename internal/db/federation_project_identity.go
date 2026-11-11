package db

import (
	"context"
	"database/sql"
	"strings"
)

// FederationProjectIdentityDependencyPrefix stores former project UIDs that a
// joined replica still needs after a reset removes its move events.
const FederationProjectIdentityDependencyPrefix = "federation_identity_dependency."

// CheckFederationProjectIdentityRetirement protects only former identities
// still referenced by another joined project's portable issue history. Archive
// and leave retain the identity; purge and merge must wait for those streams to
// detach. Local history covered by an adoption baseline creates no dependency.
func CheckFederationProjectIdentityRetirement(ctx context.Context, tx federationHistoryQuery, project Project) error {
	joinedIDs, joinedUIDs := map[int64]bool{}, map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.uid FROM projects p
 JOIN federation_bindings b ON b.project_id=p.id WHERE p.id<>$1`, project.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var uid string
		if err := rows.Scan(&id, &uid); err != nil {
			_ = rows.Close()
			return err
		}
		joinedIDs[id], joinedUIDs[uid] = true, true
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(joinedIDs) == 0 {
		return nil
	}
	for dependentUID := range joinedUIDs {
		key := FederationProjectIdentityDependencyPrefix + dependentUID + "." + project.UID
		var depends bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta WHERE key=$1)`, key).Scan(&depends); err != nil {
			return err
		}
		if depends {
			return ErrFederationProjectIdentityRequired
		}
	}
	owners := map[string]int64{}
	cutoffs := map[string]int64{}
	var afterID int64
	for {
		const limit = 1000
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.issue_uid,e.project_id,e.type,e.payload,i.project_id FROM events e
 JOIN issues i ON i.uid=e.issue_uid
 WHERE (e.project_id=$1 OR e.type='issue.moved') AND e.type<>'sync.reset_required' AND e.id>$2
 ORDER BY e.id LIMIT $3`, project.ID, afterID, limit)
		if err != nil {
			return err
		}
		var events []Event
		for rows.Next() {
			var event Event
			var issueUID, payload string
			var ownerID int64
			if err := rows.Scan(&event.ID, &issueUID, &event.ProjectID, &event.Type, &payload, &ownerID); err != nil {
				_ = rows.Close()
				return err
			}
			event.IssueUID, event.Payload = new(issueUID), payload
			owners[issueUID] = ownerID
			events = append(events, event)
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := loadFederationMoveHistoryCutoffs(ctx, tx, events, cutoffs); err != nil {
			return err
		}
		for _, event := range events {
			afterID = event.ID
			if event.ID < cutoffs[*event.IssueUID] {
				continue
			}
			payload := PayloadMap([]byte(event.Payload))
			from, _ := StringValue(payload["from_project_uid"])
			nativeMove := event.Type == "issue.moved" && payload["snapshot"] != nil
			// Match the ordinary native feed: current ownership, original
			// envelope, or an outbound move from a still-joined source/transit.
			exposed := joinedIDs[owners[*event.IssueUID]] || joinedIDs[event.ProjectID] || (nativeMove && joinedUIDs[from])
			if !exposed {
				continue
			}
			if event.ProjectID == project.ID {
				return ErrFederationProjectIdentityRequired
			}
			if !nativeMove {
				continue // Bare local moves do not establish native federation lineage.
			}
			for _, key := range []string{"from_project_uid", "to_project_uid"} {
				endpoint, _ := StringValue(payload[key])
				if endpoint == project.UID {
					return ErrFederationProjectIdentityRequired
				}
			}
		}
		if len(events) < limit {
			return nil
		}
	}
}

// PreserveFederationProjectIdentityDependencies records native move endpoints
// before a reset deletes their events. The dependency belongs to the joined
// project stream and is retired when that stream leaves federation.
func PreserveFederationProjectIdentityDependencies(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var dependentUID string
	var role sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT p.uid,b.role FROM projects p
 LEFT JOIN federation_bindings b ON b.project_id=p.id WHERE p.id=$1`, projectID).Scan(&dependentUID, &role); err != nil {
		return err
	}
	if !role.Valid || role.String != string(FederationRoleSpoke) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value='1' WHERE substr(key,1,length($1))=$1 AND value='pull'`, FederationProjectIdentityDependencyPrefix); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.payload FROM events e
 WHERE e.type='issue.moved' AND e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)`, projectID)
	if err != nil {
		return err
	}
	dependencies := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		payload := PayloadMap([]byte(raw))
		snapshot, hasSnapshot := payload["snapshot"]
		if !hasSnapshot || strings.TrimSpace(string(snapshot)) == "null" {
			continue // A local snapshotless move has no portable federation lineage.
		}
		for _, key := range []string{"from_project_uid", "to_project_uid"} {
			projectUID, _ := StringValue(payload[key])
			if projectUID != "" && projectUID != dependentUID {
				dependencies[projectUID] = struct{}{}
			}
		}
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for requiredUID := range dependencies {
		if err := recordFederationProjectIdentityDependency(ctx, tx, dependentUID, requiredUID); err != nil {
			return err
		}
	}
	return nil
}

// recordFederationProjectIdentityDependency holds an identity for a joined stream.
func recordFederationProjectIdentityDependency(ctx context.Context, tx *sql.Tx, dependentUID, requiredUID string) error {
	if requiredUID == "" || requiredUID == dependentUID {
		return nil
	}
	key := FederationProjectIdentityDependencyPrefix + dependentUID + "." + requiredUID
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,'1') ON CONFLICT(key) DO UPDATE SET value='1'`, key)
	return err
}

// A pull-only hold can be retired when all matching portable history is
// explicitly purged. Older and reset-preserved holds keep their original value.
func recordPulledFederationProjectIdentityDependency(ctx context.Context, tx *sql.Tx, dependentUID, requiredUID string) error {
	if requiredUID == "" || requiredUID == dependentUID {
		return nil
	}
	key := FederationProjectIdentityDependencyPrefix + dependentUID + "." + requiredUID
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,'pull') ON CONFLICT(key) DO NOTHING`, key)
	return err
}

type federationIdentityPurgeQuery interface {
	federationHistoryQuery
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// RetirePurgedFederationIdentityDependencies removes pull-only holds only when
// no remaining issue envelope or move endpoint references that identity. Reset
// holds and legacy holds are deliberately never inferred from missing history.
func RetirePurgedFederationIdentityDependencies(ctx context.Context, tx federationIdentityPurgeQuery) error {
	rows, err := tx.QueryContext(ctx, `SELECT key FROM meta WHERE substr(key,1,length($1))=$1 AND value='pull'`, FederationProjectIdentityDependencyPrefix)
	if err != nil {
		return err
	}
	keys := map[string][]string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		parts := strings.Split(strings.TrimPrefix(key, FederationProjectIdentityDependencyPrefix), ".")
		if len(parts) == 2 {
			keys[parts[1]] = append(keys[parts[1]], key)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT p.uid,e.type,e.payload FROM events e JOIN projects p ON p.id=e.project_id WHERE e.type LIKE 'issue.%' OR e.issue_uid IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var projectUID, typ, payload string
		if err := rows.Scan(&projectUID, &typ, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		delete(keys, projectUID)
		if typ == "issue.moved" {
			m := PayloadMap([]byte(payload))
			for _, field := range []string{"from_project_uid", "to_project_uid"} {
				uid, _ := StringValue(m[field])
				delete(keys, uid)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, group := range keys {
		for _, key := range group {
			if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1 AND value='pull'`, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ClearFederationProjectIdentityDependencies retires the identity holds owned
// by a replica after its federation stream has been detached.
func ClearFederationProjectIdentityDependencies(ctx context.Context, tx *sql.Tx, dependentUID string) error {
	prefix := FederationProjectIdentityDependencyPrefix + dependentUID + "."
	_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE substr(key,1,length($1))=$1`, prefix)
	return err
}
