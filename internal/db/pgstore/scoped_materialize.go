package pgstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) loadScopedMaterializationTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey) (db.ScopedMaterialization, error) {
	var result db.ScopedMaterialization
	view, _, err := s.scopedDescriptorTx(ctx, tx, key)
	if err != nil {
		return result, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint_bytes FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&raw); err != nil {
		return result, err
	}
	if len(raw) == 0 {
		return result, db.ErrNotFound
	}
	var checkpoint db.ScopedCheckpoint
	if err := db.DecodeScopedRecord(raw, &checkpoint); err != nil {
		return result, err
	}
	pins := []db.RootKeyPin{}
	pinRows, err := tx.QueryContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys ORDER BY project_uid,key_id`)
	if err != nil {
		return result, err
	}
	for pinRows.Next() {
		pin, e := scanRootPin(pinRows)
		if e != nil {
			err = e
			break
		}
		pins = append(pins, pin)
	}
	if err == nil {
		err = pinRows.Err()
	}
	_ = pinRows.Close()
	if err != nil {
		return result, err
	}
	if err := db.VerifyScopedCheckpoint(checkpoint, view, pins); err != nil {
		return result, err
	}
	batches := []db.ScopedBatch{}
	rows, err := tx.QueryContext(ctx, `SELECT batch_bytes FROM federation_scoped_batches WHERE view_uid=$1 AND generation=$2 ORDER BY sequence`, key.ViewUID, key.Generation)
	if err != nil {
		return result, err
	}
	cut := checkpoint.Cut
	for rows.Next() {
		var body []byte
		var b db.ScopedBatch
		if err = rows.Scan(&body); err != nil {
			break
		}
		if err = db.DecodeScopedRecord(body, &b); err != nil {
			break
		}
		if err = db.VerifyScopedBatch(b, view, pins); err != nil {
			break
		}
		batches = append(batches, b)
		if b.Cut.Sequence > cut.Sequence {
			cut = b.Cut
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	inventory, err := db.ScopedRecordInventory(checkpoint, batches)
	if err != nil {
		return result, err
	}
	actual := map[string]db.ScopedEffect{}
	rows, err = tx.QueryContext(ctx, `SELECT view_uid,generation,collection,record_key,project_uid,subject_uid,peer_project_uid,peer_uid,visible_revision,present,record_bytes,digest FROM federation_scoped_records WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var r db.ScopedRecordExport
		var present int
		err = rows.Scan(&r.ViewUID, &r.Generation, &r.Collection, &r.RecordKey, &r.ProjectUID, &r.SubjectUID, &r.PeerProjectUID, &r.PeerUID, &r.VisibleRevision, &present, &r.RecordBytes, &r.Digest)
		if err != nil {
			break
		}
		r.Present = present == 1
		if present != 0 && present != 1 {
			err = errors.New("invalid scoped record presence")
			break
		}
		if err = db.ValidateScopedRecordEnvelope(r); err != nil {
			break
		}
		var e db.ScopedEffect
		if err = db.DecodeScopedRecord(r.RecordBytes, &e); err != nil {
			break
		}
		actual[r.Collection+"/"+r.RecordKey] = e
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	for _, e := range inventory {
		id := e.Collection + "/" + e.Key
		row, ok := actual[id]
		if !ok {
			if e.Action == "remove" && e.Revision <= checkpoint.Cut.Sequence {
				continue
			}
			return result, errors.New("missing signed scoped record")
		}
		retained, err := db.ScopedAssertionJSON(row)
		if err != nil {
			return result, err
		}
		signed, err := db.ScopedAssertionJSON(e)
		if err != nil {
			return result, err
		}
		if !bytes.Equal(retained, signed) {
			return result, errors.New("scoped row differs from authenticated inventory")
		}
		delete(actual, id)
	}
	if len(actual) != 0 {
		return result, errors.New("unexpected scoped row")
	}
	result.Collections, err = db.ScopedCollectionsFromInventory(inventory)
	if err != nil {
		return result, err
	}
	result.Fence = db.ScopedOwnerFence{View: view, Cut: cut, Owners: []db.ScopedProjectExport{}}
	for _, p := range view.Projects {
		owner, err := s.scopedProjectOwnerTx(ctx, tx, p.ProjectUID)
		if err != nil {
			return result, err
		}
		result.Fence.Owners = append(result.Fence.Owners, owner)
	}
	return result, nil
}

// validateScopedOwnedTx checks only this generation's unfenced native owners.
// It consumes the authenticated inventory, never folds canonical events or
// borrows live binding/account authority for an offline owner restore.
func (s *Store) validateScopedOwnedTx(ctx context.Context, tx *sql.Tx, materialized db.ScopedMaterialization) error {
	projects := make(map[string]db.ScopedProjectState, len(materialized.Collections.Projects))
	for _, project := range materialized.Collections.Projects {
		projects[project.UID] = project
	}
	issues := make(map[db.ScopedEndpoint]db.ScopedIssueState, len(materialized.Collections.Issues))
	for _, issue := range materialized.Collections.Issues {
		issues[db.ScopedEndpoint{ProjectUID: issue.ProjectUID, IssueUID: issue.UID}] = issue
	}
	// Comment identity is project/kind/UID; membership in an issue is a
	// signed scalar too, not inferred from a matching global comment UID.
	type commentIdentity struct{ project, uid string }
	comments := make(map[commentIdentity]db.ScopedCommentState, len(materialized.Collections.Comments))
	for _, comment := range materialized.Collections.Comments {
		comments[commentIdentity{comment.ProjectUID, comment.UID}] = comment
	}
	for _, owner := range materialized.Fence.Owners {
		if owner.ServingFenced || owner.OwnerViewUID == nil || owner.OwnerGeneration == nil ||
			*owner.OwnerViewUID != materialized.Fence.View.Key.ViewUID ||
			*owner.OwnerGeneration != materialized.Fence.View.Key.Generation {
			continue
		}
		// A catalog row can exist before any signed project content. Its local
		// Name remains a routing alias; only represented metadata is compared.
		if project, present := projects[owner.ProjectUID]; present {
			var metadata db.JSONBlob
			var deleted any
			if err := tx.QueryRowContext(ctx, `SELECT metadata,deleted_at FROM projects WHERE uid=$1`, owner.ProjectUID).Scan(&metadata, &deleted); err != nil {
				return err
			}
			want, err := db.ScopedAssertionJSON(project.Metadata)
			if err != nil {
				return err
			}
			got, err := db.ScopedAssertionJSON(metadata)
			if err != nil {
				return err
			}
			if deleted != nil || !bytes.Equal(want, got) {
				return errors.New("native owned project differs from authenticated projection")
			}
		}
		native, err := s.scopedNativeIssuesTx(ctx, tx, owner.ProjectUID)
		if err != nil {
			return err
		}
		for _, issue := range native {
			key := db.ScopedEndpoint{ProjectUID: issue.ProjectUID, IssueUID: issue.UID}
			want, present := issues[key]
			if !present {
				return errors.New("native owned issue has no authenticated projection")
			}
			expected, err := db.ScopedIssueAssertion(want)
			if err != nil {
				return err
			}
			actual, err := db.ScopedIssueAssertion(issue)
			if err != nil {
				return err
			}
			if !bytes.Equal(expected, actual) {
				return errors.New("native owned issue differs from authenticated projection")
			}
			delete(issues, key)
		}
		for key := range issues {
			if key.ProjectUID == owner.ProjectUID {
				return errors.New("authenticated owned issue missing from native materialization")
			}
		}
		nativeComments, err := s.scopedNativeCommentsTx(ctx, tx, owner.ProjectUID)
		if err != nil {
			return err
		}
		for _, comment := range nativeComments {
			key := commentIdentity{comment.ProjectUID, comment.UID}
			want, present := comments[key]
			if !present {
				return errors.New("native owned comment has no authenticated projection")
			}
			want.CreatedAt = want.CreatedAt.UTC()
			comment.CreatedAt = comment.CreatedAt.UTC()
			expected, err := db.ScopedAssertionJSON(want)
			if err != nil {
				return err
			}
			actual, err := db.ScopedAssertionJSON(comment)
			if err != nil {
				return err
			}
			if !bytes.Equal(expected, actual) {
				return errors.New("native owned comment differs from authenticated projection")
			}
			delete(comments, key)
		}
		for key := range comments {
			if key.project == owner.ProjectUID {
				return errors.New("authenticated owned comment missing from native materialization")
			}
		}
	}
	return s.validateScopedOwnedRelationsTx(ctx, tx, materialized)
}

// Corroborate only exact unfenced owners. Labels and links carry local
// bookkeeping fields, so compare their signed identities rather than authors
// or local timestamps. This runs in the same transaction as scalar validation.
func (s *Store) validateScopedOwnedRelationsTx(ctx context.Context, tx *sql.Tx, m db.ScopedMaterialization) error {
	owned := map[string]bool{}
	for _, owner := range m.Fence.Owners {
		if !owner.ServingFenced && owner.OwnerViewUID != nil && owner.OwnerGeneration != nil &&
			*owner.OwnerViewUID == m.Fence.View.Key.ViewUID && *owner.OwnerGeneration == m.Fence.View.Key.Generation {
			owned[owner.ProjectUID] = true
		}
	}
	labels := map[db.ScopedLabelState]bool{}
	for _, label := range m.Collections.Labels {
		if owned[label.ProjectUID] {
			labels[label] = true
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.uid,i.uid,l.label FROM issue_labels l JOIN issues i ON i.id=l.issue_id JOIN projects p ON p.id=i.project_id WHERE EXISTS(SELECT 1 FROM federation_scoped_projects o WHERE o.project_uid=p.uid AND o.owner_view_uid=$1 AND o.owner_generation=$2 AND o.serving_fenced=0)`, m.Fence.View.Key.ViewUID, m.Fence.View.Key.Generation)
	if err != nil {
		return err
	}
	for rows.Next() {
		var label db.ScopedLabelState
		if err = rows.Scan(&label.ProjectUID, &label.IssueUID, &label.Label); err != nil {
			break
		}
		if !labels[label] {
			err = errors.New("native owned label has no authenticated projection")
			break
		}
		delete(labels, label)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(labels) != 0 {
		return errors.New("authenticated owned label missing from native materialization")
	}
	issues := map[string]db.ScopedIssueState{}
	for _, issue := range m.Collections.Issues {
		issues[issue.UID] = issue
	}
	edges, err := scopedMaterializedEdges(m.Collections.References, issues)
	if err != nil {
		return err
	}
	for key, edge := range edges {
		if !owned[issues[edge.from].ProjectUID] && !owned[issues[edge.to].ProjectUID] {
			delete(edges, key)
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT l.from_issue_uid,l.to_issue_uid,l.type FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN projects fp ON fp.id=f.project_id JOIN issues t ON t.id=l.to_issue_id JOIN projects tp ON tp.id=t.project_id WHERE EXISTS(SELECT 1 FROM federation_scoped_projects o WHERE o.owner_view_uid=$1 AND o.owner_generation=$2 AND o.serving_fenced=0 AND (o.project_uid=fp.uid OR o.project_uid=tp.uid))`, m.Fence.View.Key.ViewUID, m.Fence.View.Key.Generation)
	if err != nil {
		return err
	}
	for rows.Next() {
		var from, to, relation string
		if err = rows.Scan(&from, &to, &relation); err != nil {
			break
		}
		key := scopedNativeEdgeKey(from, to, relation)
		if _, present := edges[key]; !present {
			err = errors.New("native owned link has no authenticated projection")
			break
		}
		delete(edges, key)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(edges) != 0 {
		return errors.New("authenticated owned link missing from native materialization")
	}
	return nil
}

// Read only the authenticated scalar fields, without native response scanners:
// validation must not register every transport-selected issue as caller-exposed.
func (s *Store) scopedNativeIssuesTx(ctx context.Context, tx *sql.Tx, project string) ([]db.ScopedIssueState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.uid,p.uid,i.short_id,i.title,i.body,i.author,i.status,i.owner,i.priority,i.assignment_expires_on,i.created_at,i.metadata,i.deleted_at FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=$1 ORDER BY i.uid`, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []db.ScopedIssueState{}
	for rows.Next() {
		var issue db.ScopedIssueState
		var expires, deleted storedNullTime
		if err := rows.Scan(&issue.UID, &issue.ProjectUID, &issue.ShortID, &issue.Title, &issue.Body, &issue.Author, &issue.Status, &issue.Owner, &issue.Priority, &expires, (*storedTime)(&issue.CreatedAt), &issue.Metadata, &deleted); err != nil {
			return nil, err
		}
		issue.AssignmentExpiresOn = expires.Time
		issue.Deleted = deleted.Time != nil
		result = append(result, issue)
	}
	return result, rows.Err()
}

// Read signed comment fields directly. Restore validation has no live caller
// prerequisite and must not hydrate response attribution or exposed-ID state.
func (s *Store) scopedNativeCommentsTx(ctx context.Context, tx *sql.Tx, project string) ([]db.ScopedCommentState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.uid,p.uid,i.uid,c.author,COALESCE(c.teammate,''),c.body,c.created_at FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE p.uid=$1 ORDER BY c.uid`, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []db.ScopedCommentState{}
	for rows.Next() {
		var comment db.ScopedCommentState
		if err := rows.Scan(&comment.UID, &comment.ProjectUID, &comment.IssueUID, &comment.Author, &comment.Teammate, &comment.Body, (*storedTime)(&comment.CreatedAt)); err != nil {
			return nil, err
		}
		result = append(result, comment)
	}
	return result, rows.Err()
}

// materializeScopedOwnedTx writes only normalized current content owned by the
// exact active generation. Root originals, local routing aliases, custody and
// canonical definition/close clocks are never synthesized or replaced here.
func (s *Store) materializeScopedOwnedTx(ctx context.Context, tx *sql.Tx, m db.ScopedMaterialization) error {
	projects := map[string]int64{}
	for _, selected := range m.Fence.View.Projects {
		owner, err := s.scopedProjectOwnerTx(ctx, tx, selected.ProjectUID)
		if err != nil {
			return err
		}
		if owner.ServingFenced || owner.OwnerViewUID == nil || owner.OwnerGeneration == nil || *owner.OwnerViewUID != m.Fence.View.Key.ViewUID || *owner.OwnerGeneration != m.Fence.View.Key.Generation {
			return db.ErrNotFound
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`, selected.ProjectUID).Scan(&id); err != nil {
			return err
		}
		projects[selected.ProjectUID] = id
	}
	// Inventory collisions, moves and omissions before any normalized INSERT
	// can reserve an identity. The selected project map contains only unfenced
	// owners of this exact view/generation, including both ends of every move.
	issues := map[string]db.ScopedIssueState{}
	for _, issue := range m.Collections.Issues {
		if _, owned := projects[issue.ProjectUID]; !owned {
			return db.ErrNotFound
		}
		var priorProject string
		err := tx.QueryRowContext(ctx, `SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, issue.UID).Scan(&priorProject)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && priorProject != issue.ProjectUID {
			if _, owned := projects[priorProject]; !owned {
				return db.ErrRemoteEventConflict
			}
		}
		issues[issue.UID] = issue
	}
	// Omitted and moved comments may still carry local or source obligations.
	// Check both before any writes. Only same-issue comments corroborated by the
	// signed destination can accompany a selected-owner issue move.
	comments := map[string]db.ScopedCommentState{}
	for _, comment := range m.Collections.Comments {
		issue, present := issues[comment.IssueUID]
		if !present || issue.ProjectUID != comment.ProjectUID {
			return db.ErrNotFound
		}
		comments[comment.UID] = comment
	}
	removedComments := []db.ScopedCommentState{}
	movedComments := []db.ScopedCommentState{}
	for _, selected := range m.Fence.View.Projects {
		rows, err := tx.QueryContext(ctx, `SELECT c.uid,i.uid FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE p.uid=$1 ORDER BY c.uid`, selected.ProjectUID)
		if err != nil {
			return err
		}
		native := []db.ScopedCommentState{}
		for rows.Next() {
			comment := db.ScopedCommentState{ProjectUID: selected.ProjectUID}
			if err = rows.Scan(&comment.UID, &comment.IssueUID); err != nil {
				break
			}
			native = append(native, comment)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, comment := range native {
			if wanted, present := comments[comment.UID]; present {
				if wanted.IssueUID != comment.IssueUID {
					return db.ErrRemoteEventConflict
				}
				if wanted.ProjectUID != comment.ProjectUID {
					movedComments = append(movedComments, comment)
				}
				continue
			}
			removedComments = append(removedComments, comment)
		}
	}
	changedIssues := []db.ScopedIssueState{}
	removed := []db.ScopedIssueState{}
	for _, selected := range m.Fence.View.Projects {
		native, err := s.scopedNativeIssuesTx(ctx, tx, selected.ProjectUID)
		if err != nil {
			return err
		}
		for _, issue := range native {
			replacement, present := issues[issue.UID]
			if present && replacement.ProjectUID == issue.ProjectUID {
				continue
			}
			// Retain the original project/UID for both omission and move checks.
			changedIssues = append(changedIssues, issue)
			if !present {
				removed = append(removed, issue)
			}
		}
	}
	checkedComments := append(slices.Clone(removedComments), movedComments...)
	inventory, err := s.loadScopedRemovalInventoryTx(ctx, tx, changedIssues, checkedComments)
	if err != nil {
		return err
	}
	for _, comment := range checkedComments {
		if err := s.scopedPreflightCommentRemovalTx(ctx, tx, comment, inventory); err != nil {
			return err
		}
	}
	for _, comment := range removedComments {
		if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE uid=$1`, comment.UID); err != nil {
			return err
		}
	}
	for _, issue := range changedIssues {
		// Every remaining comment on a moved issue has the same signed issue
		// identity and passed its original-project obligation guards above.
		// Omission keeps the comments-existence guard; all other guards always run.
		replacement, present := issues[issue.UID]
		allowCurrentComments := present && replacement.ProjectUID != issue.ProjectUID
		if err := s.scopedPreflightIssueRemovalTx(ctx, tx, m.Fence.View.Key, issue, inventory, allowCurrentComments); err != nil {
			return err
		}
	}
	affectedProjects := []string{}
	for _, issue := range changedIssues {
		replacement, present := issues[issue.UID]
		if !present {
			continue // Omission follows the checked removal path below.
		}
		var nativeID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1 AND project_id=$2`, issue.UID, projects[issue.ProjectUID]).Scan(&nativeID); err != nil {
			return err
		}
		// Invalidate peers before relocating this exact checked native subject.
		affected, err := moveScopedCanonicalCloseIssueTx(ctx, tx, db.Issue{ID: nativeID, UID: issue.UID, ProjectUID: issue.ProjectUID}, replacement.ProjectUID)
		if err != nil {
			return err
		}
		affectedProjects = append(affectedProjects, affected...)
	}
	for _, issue := range removed {
		var nativeID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1 AND project_id=$2`, issue.UID, projects[issue.ProjectUID]).Scan(&nativeID); err != nil {
			return err
		}
		// Invalidate derived peers while the native issue and links still exist.
		// The shared native primitive preserves retained writer and source bytes.
		affected, err := removeScopedCanonicalCloseIssueTx(ctx, tx, db.Issue{ID: nativeID, UID: issue.UID, ProjectUID: issue.ProjectUID})
		if err != nil {
			return err
		}
		affectedProjects = append(affectedProjects, affected...)
		// These are normalized rows under the exact checked owner. No event,
		// source, result, receipt, mapping or retained generation is removed.
		for _, statement := range []string{
			`DELETE FROM links WHERE from_issue_uid=$1 OR to_issue_uid=$1`,
			`DELETE FROM issue_labels WHERE issue_id=(SELECT id FROM issues WHERE uid=$1)`,
			`DELETE FROM issues WHERE uid=$1`,
		} {
			if _, err := tx.ExecContext(ctx, statement, issue.UID); err != nil {
				return err
			}
		}
	}
	if len(affectedProjects) != 0 {
		if err := markScopedCanonicalDirtyTx(ctx, tx, affectedProjects...); err != nil {
			return err
		}
	}
	for _, project := range m.Collections.Projects {
		id, owned := projects[project.UID]
		if !owned {
			return db.ErrNotFound
		}
		native, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, id))
		if err != nil {
			return err
		}
		if err := db.ValidateImportRecords([]db.ImportRecord{&db.ProjectExport{ID: id, UID: project.UID, Name: native.Name, CreatedAt: native.CreatedAt.UTC().Format(time.RFC3339Nano), Metadata: jsontext.Value(project.Metadata), Revision: native.Revision}}); err != nil {
			return err
		}
		before, err := db.ScopedAssertionJSON(native.Metadata)
		if err != nil {
			return err
		}
		after, err := db.ScopedAssertionJSON(project.Metadata)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET metadata=$1,revision=revision+1 WHERE id=$2`, string(project.Metadata), id); err != nil {
				return err
			}
		}
	}
	for _, issue := range m.Collections.Issues {
		project, owned := projects[issue.ProjectUID]
		if !owned {
			return db.ErrNotFound
		}
		issues[issue.UID] = issue
		record := &db.IssueExport{ID: 1, UID: issue.UID, ProjectID: project, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Status: issue.Status, Owner: issue.Owner, Priority: issue.Priority, Author: issue.Author, CreatedAt: issue.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: issue.CreatedAt.UTC().Format(time.RFC3339Nano), AssignmentExpiresOn: db.ScopedCaptureTime(issue.AssignmentExpiresOn), Metadata: jsontext.Value(issue.Metadata), Revision: 1, ContentRevision: 1}
		if issue.Deleted {
			record.DeletedAt = &record.CreatedAt // local tombstone marker, not a signed deletion timestamp
		}
		if err := db.ValidateImportRecords([]db.ImportRecord{record}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,status,owner,assignment_expires_on,priority,author,created_at,updated_at,deleted_at,metadata,revision,content_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13,1,1) ON CONFLICT(uid) DO UPDATE SET project_id=excluded.project_id,short_id=excluded.short_id,title=excluded.title,body=excluded.body,status=excluded.status,owner=excluded.owner,assignment_expires_on=excluded.assignment_expires_on,priority=excluded.priority,author=excluded.author,created_at=excluded.created_at,deleted_at=excluded.deleted_at,metadata=excluded.metadata,closed_reason=CASE WHEN excluded.status='open' THEN NULL ELSE issues.closed_reason END,closed_at=CASE WHEN excluded.status='open' THEN NULL ELSE issues.closed_at END,revision=issues.revision+1,content_revision=CASE WHEN issues.title<>excluded.title OR issues.body<>excluded.body THEN issues.content_revision+1 ELSE issues.content_revision END`, issue.UID, project, issue.ShortID, issue.Title, issue.Body, issue.Status, issue.Owner, record.AssignmentExpiresOn, issue.Priority, issue.Author, record.CreatedAt, record.DeletedAt, string(issue.Metadata)); err != nil {
			return err
		}
	}
	for _, comment := range m.Collections.Comments {
		if issue, present := issues[comment.IssueUID]; !present || issue.ProjectUID != comment.ProjectUID {
			return db.ErrNotFound
		}
		var issueID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1`, comment.IssueUID).Scan(&issueID); err != nil {
			return err
		}
		record := &db.CommentExport{ID: 1, UID: comment.UID, IssueID: issueID, Author: comment.Author, Teammate: comment.Teammate, Body: comment.Body, CreatedAt: comment.CreatedAt.UTC().Format(time.RFC3339Nano)}
		if err := db.ValidateImportRecords([]db.ImportRecord{record}); err != nil {
			return err
		}
		var priorIssue string
		err := tx.QueryRowContext(ctx, `SELECT i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1`, comment.UID).Scan(&priorIssue)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && priorIssue != comment.IssueUID {
			return db.ErrRemoteEventConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,teammate,body,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(uid) DO UPDATE SET author=excluded.author,teammate=excluded.teammate,body=excluded.body,created_at=excluded.created_at`, comment.UID, issueID, comment.Author, comment.Teammate, comment.Body, record.CreatedAt); err != nil {
			return err
		}
	}
	for uid, issue := range issues {
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id=(SELECT id FROM issues WHERE uid=$1)`, uid); err != nil {
			return err
		}
		for _, label := range m.Collections.Labels {
			if label.IssueUID == uid && label.ProjectUID == issue.ProjectUID {
				if _, err := tx.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author,created_at) SELECT id,$1,$2,$3 FROM issues WHERE uid=$4`, label.Label, issue.Author, issue.CreatedAt.UTC().Format(time.RFC3339Nano), uid); err != nil {
					return err
				}
			}
		}
	}
	if err := s.scopedMaterializeReferencesTx(ctx, tx, m, issues); err != nil {
		return err
	}
	// Typed definitions, provenance, manifests and nullable slots remain in the
	// verified native scoped records. They do not manufacture canonical cron
	// winner clocks, source events or local artifact/blob authority.
	return s.validateScopedOwnedTx(ctx, tx, m)
}

// scopedRemovalInventory is used only within one native materialization
// transaction. It records retention obligations, never serving authority.
type scopedRemovalInventory struct {
	issues   map[db.ScopedEndpoint]bool
	comments map[scopedCommentEndpoint]bool
}

type scopedCommentEndpoint struct{ project, issue, comment string }

// Decode retained originals once for all changed endpoints in this pass.
// Ordinary updates with no omission/move do not scan or decode retained rows.
func (s *Store) loadScopedRemovalInventoryTx(ctx context.Context, tx *sql.Tx, issues []db.ScopedIssueState, comments []db.ScopedCommentState) (scopedRemovalInventory, error) {
	inventory := scopedRemovalInventory{issues: map[db.ScopedEndpoint]bool{}, comments: map[scopedCommentEndpoint]bool{}}
	if len(issues) == 0 && len(comments) == 0 {
		return inventory, nil
	}
	issueTargets := map[db.ScopedEndpoint]bool{}
	issueUIDs := map[string][]db.ScopedEndpoint{}
	for _, issue := range issues {
		endpoint := db.ScopedEndpoint{ProjectUID: issue.ProjectUID, IssueUID: issue.UID}
		issueTargets[endpoint] = true
		issueUIDs[issue.UID] = append(issueUIDs[issue.UID], endpoint)
	}
	commentsByIssue := map[db.ScopedEndpoint][]scopedCommentEndpoint{}
	commentIssueUIDs := map[string]bool{}
	commentsByUID := map[string][]scopedCommentEndpoint{}
	for _, comment := range comments {
		endpoint := scopedCommentEndpoint{comment.ProjectUID, comment.IssueUID, comment.UID}
		issue := db.ScopedEndpoint{ProjectUID: comment.ProjectUID, IssueUID: comment.IssueUID}
		commentsByIssue[issue] = append(commentsByIssue[issue], endpoint)
		commentIssueUIDs[comment.IssueUID] = true
		commentsByUID[comment.UID] = append(commentsByUID[comment.UID], endpoint)
	}
	operationOwed := map[string]bool{}
	owed := func(operation sql.NullString) (bool, error) {
		if !operation.Valid {
			return true, nil // exact private conversion original still retained
		}
		value, present := operationOwed[operation.String]
		if !present {
			return false, sql.ErrNoRows
		}
		return value, nil
	}
	visit := func(operation sql.NullString, event db.RemoteEvent, includeIssues bool) error {
		primary := db.ScopedEndpoint{ProjectUID: event.ProjectUID}
		if event.IssueUID != nil {
			primary.IssueUID = *event.IssueUID
		}
		if includeIssues {
			if issueTargets[primary] {
				pending, err := owed(operation)
				if err != nil {
					return err
				}
				inventory.issues[primary] = inventory.issues[primary] || pending
			}
			if event.RelatedIssueUID != nil {
				for _, endpoint := range issueUIDs[*event.RelatedIssueUID] {
					pending, err := owed(operation)
					if err != nil {
						return err
					}
					inventory.issues[endpoint] = inventory.issues[endpoint] || pending
				}
			}
		}
		if len(commentsByUID) == 0 {
			return nil
		}
		switch event.Type {
		case "issue.created", "issue.snapshot":
			for _, endpoint := range commentsByIssue[primary] {
				pending, err := owed(operation)
				if err != nil {
					return err
				}
				inventory.comments[endpoint] = inventory.comments[endpoint] || pending
			}
		case "issue.commented", "issue.comment_edited":
			peerPossible := event.RelatedIssueUID != nil && commentIssueUIDs[*event.RelatedIssueUID]
			if len(commentsByIssue[primary]) == 0 && !peerPossible {
				return nil
			}
			var payload struct {
				CommentUID string `json:"comment_uid"`
				ReplyToUID string `json:"reply_to_uid"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			for _, endpoint := range commentsByUID[payload.CommentUID] {
				if endpoint.project == primary.ProjectUID && endpoint.issue == primary.IssueUID {
					pending, err := owed(operation)
					if err != nil {
						return err
					}
					inventory.comments[endpoint] = inventory.comments[endpoint] || pending
				}
			}
			for _, endpoint := range commentsByUID[payload.ReplyToUID] {
				subject := endpoint.project == primary.ProjectUID && endpoint.issue == primary.IssueUID
				peer := event.RelatedIssueUID != nil && *event.RelatedIssueUID == endpoint.issue
				if subject || peer {
					pending, err := owed(operation)
					if err != nil {
						return err
					}
					inventory.comments[endpoint] = inventory.comments[endpoint] || pending
				}
			}
		}
		return nil
	}
	// Conversion obligations have no object-UID-leading index. Inventory them
	// once, preserving the same nonsettled-object condition for each operation.
	conversionOwed := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT object_uid FROM federation_conversion_obligations WHERE state<>'settled'`)
	if err != nil {
		return inventory, err
	}
	for rows.Next() {
		var objectUID string
		if err = rows.Scan(&objectUID); err != nil {
			break
		}
		conversionOwed[objectUID] = true
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return inventory, err
	}
	// accepted_private is terminal history, not a pending overlay. Missing
	// terminal results, pending/conflict overlays, result ACKs and conversion
	// obligations independently retain work that is still owed.
	query := `SELECT operation_uid,terminal_bytes IS NULL OR overlay_state IN ('pending','conflict') OR EXISTS(SELECT 1 FROM federation_source_custody c WHERE c.operation_uid=o.operation_uid AND c.result_acknowledged=0)`
	if len(comments) != 0 {
		query += `,manifest_bytes`
	}
	query += ` FROM federation_source_operations o`
	rows, err = tx.QueryContext(ctx, query)
	if err != nil {
		return inventory, err
	}
	for rows.Next() {
		var operation string
		var pending bool
		var raw []byte
		if len(comments) == 0 {
			err = rows.Scan(&operation, &pending)
		} else {
			err = rows.Scan(&operation, &pending, &raw)
		}
		if err != nil {
			break
		}
		operationOwed[operation] = pending || conversionOwed[operation]
		if len(comments) != 0 {
			var submission db.SourceSubmission
			submission, err = db.DecodeScopedSourceSubmission(raw)
			if err != nil {
				break
			}
			for _, event := range submission.Originals {
				// Issue removal previously used dependencies/private originals;
				// complete manifest inspection adds only the existing comment check.
				if err = visit(sql.NullString{String: operation, Valid: true}, event, false); err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return inventory, err
	}
	if len(issues) != 0 {
		rows, err = tx.QueryContext(ctx, `SELECT operation_uid,project_uid,issue_uid,state FROM federation_source_dependencies`)
		if err != nil {
			return inventory, err
		}
		for rows.Next() {
			var operation, state string
			var endpoint db.ScopedEndpoint
			if err = rows.Scan(&operation, &endpoint.ProjectUID, &endpoint.IssueUID, &state); err != nil {
				break
			}
			if !issueTargets[endpoint] {
				continue
			}
			if state == "pending" {
				inventory.issues[endpoint] = true
			} else {
				var pending bool
				pending, err = owed(sql.NullString{String: operation, Valid: true})
				if err != nil {
					break
				}
				inventory.issues[endpoint] = inventory.issues[endpoint] || pending
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return inventory, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT operation_uid,event_bytes FROM federation_private_sources`)
	if err != nil {
		return inventory, err
	}
	for rows.Next() {
		var operation sql.NullString
		var raw []byte
		if err = rows.Scan(&operation, &raw); err != nil {
			break
		}
		var event db.RemoteEvent
		event, err = db.DecodeRelaySourceEvent(raw)
		if err != nil {
			break
		}
		if err = visit(operation, event, true); err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return inventory, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT request_bytes FROM federation_control_custody WHERE state='pending' OR result_acknowledged=0`)
	if err != nil {
		return inventory, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return inventory, err
		}
		var request db.ScopedControlRequest
		if err := db.DecodeScopedRecord(raw, &request); err != nil {
			return inventory, err
		}
		if issueTargets[request.Subject] {
			inventory.issues[request.Subject] = true
		}
		for _, endpoint := range commentsByIssue[request.Subject] {
			inventory.comments[endpoint] = true
		}
	}
	return inventory, rows.Err()
}

// Only native normalized comment rows are removable. Canonical originals,
// mappings, preparation witnesses and pending typed source/control obligations
// remain independent authority/recovery data and are never changed here.
func (s *Store) scopedPreflightCommentRemovalTx(ctx context.Context, tx *sql.Tx, comment db.ScopedCommentState, inventory scopedRemovalInventory) error {
	var local bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM events WHERE issue_uid=$1 OR issue_id=(SELECT id FROM issues WHERE uid=$1))
 OR EXISTS(SELECT 1 FROM import_mappings WHERE comment_id=(SELECT id FROM comments WHERE uid=$2))
 OR EXISTS(SELECT 1 FROM federation_entity_provenance WHERE project_uid=$3 AND kind='comment' AND entity_uid=$2)
 OR EXISTS(SELECT 1 FROM federation_predicate_incarnations WHERE project_uid=$3 AND subject_uid=$2 AND predicate_kind LIKE 'comment.%')
 OR EXISTS(SELECT 1 FROM federation_mutation_expectations WHERE project_uid=$3 AND subject_uid IN ($1,$2) AND consumed_operation_uid IS NULL)
 OR EXISTS(SELECT 1 FROM meta WHERE key=$4)`, comment.IssueUID, comment.UID, comment.ProjectUID, db.PendingCreationMetadataPrefix+comment.ProjectUID+".comment."+comment.UID).Scan(&local); err != nil {
		return err
	}
	if local {
		return db.ErrScopedCapturePending
	}
	if inventory.comments[scopedCommentEndpoint{comment.ProjectUID, comment.IssueUID, comment.UID}] {
		return db.ErrScopedCapturePending
	}
	return nil
}

// scopedPreflightIssueRemovalTx distinguishes an authoritative current
// omission/move from obligations that still need this native subject. The sole
// content exception is already checked same-issue comments accompanying a move.
// It never deletes or rewrites retained operation/private/result/recovery rows.
func (s *Store) scopedPreflightIssueRemovalTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, issue db.ScopedIssueState, inventory scopedRemovalInventory, allowCurrentComments bool) error {
	blockComments := 1
	if allowCurrentComments {
		blockComments = 0
	}
	var local bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM events WHERE issue_uid=$1 OR related_issue_uid=$1 OR issue_id=(SELECT id FROM issues WHERE uid=$1) OR related_issue_id=(SELECT id FROM issues WHERE uid=$1))
 OR EXISTS(SELECT 1 FROM import_mappings WHERE issue_id=(SELECT id FROM issues WHERE uid=$1) OR link_id IN (SELECT id FROM links WHERE from_issue_uid=$1 OR to_issue_uid=$1))
 OR EXISTS(SELECT 1 FROM comments WHERE issue_id=(SELECT id FROM issues WHERE uid=$1) AND $6<>0)
 OR EXISTS(SELECT 1 FROM issue_claims WHERE issue_uid=$1)
 OR EXISTS(SELECT 1 FROM pending_claim_requests WHERE issue_uid=$1)
 OR EXISTS(SELECT 1 FROM issues WHERE uid=$1 AND recurrence_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM recurrences WHERE last_materialized_uid=$1)
 OR EXISTS(SELECT 1 FROM cron_runs WHERE issue_uid=$1)
 OR EXISTS(SELECT 1 FROM federation_embedding_artifacts WHERE issue_uid=$1)
 OR EXISTS(SELECT 1 FROM federation_entity_provenance WHERE project_uid=$2 AND kind='issue' AND entity_uid=$1)
 OR EXISTS(SELECT 1 FROM federation_canonical_close_state WHERE project_uid=$2 AND issue_uid=$1)
 OR EXISTS(SELECT 1 FROM federation_predicate_incarnations WHERE project_uid=$2 AND subject_uid=$1 AND predicate_kind LIKE 'issue.%')
 OR EXISTS(SELECT 1 FROM federation_mutation_expectations WHERE project_uid=$2 AND subject_uid=$1 AND consumed_operation_uid IS NULL)
 OR EXISTS(SELECT 1 FROM meta WHERE key=$3)
 OR EXISTS(SELECT 1 FROM links l JOIN issues peer ON peer.uid=CASE WHEN l.from_issue_uid=$1 THEN l.to_issue_uid ELSE l.from_issue_uid END JOIN projects p ON p.id=peer.project_id LEFT JOIN federation_scoped_projects o ON o.project_uid=p.uid WHERE (l.from_issue_uid=$1 OR l.to_issue_uid=$1) AND (o.owner_view_uid IS NULL OR o.owner_view_uid<>$4 OR o.owner_generation<>$5 OR o.serving_fenced<>0))`, issue.UID, issue.ProjectUID, db.PendingCreationMetadataPrefix+issue.ProjectUID+".issue."+issue.UID, key.ViewUID, key.Generation, blockComments).Scan(&local); err != nil {
		return err
	}
	if local {
		return db.ErrScopedCapturePending
	}

	if inventory.issues[db.ScopedEndpoint{ProjectUID: issue.ProjectUID, IssueUID: issue.UID}] {
		return db.ErrScopedCapturePending
	}
	return nil
}

func scopedNativeEdgeKey(from, to, relation string) string {
	if relation == "related" && from > to {
		from, to = to, from
	}
	return from + ":" + to + ":" + relation
}

type scopedNativeEdge struct{ from, to, relation string }

// Installation and restore corroboration use the same live-endpoint mapping.
func scopedMaterializedEdges(references []db.ScopedReference, issues map[string]db.ScopedIssueState) (map[string]scopedNativeEdge, error) {
	wanted := map[string]scopedNativeEdge{}
	for _, reference := range references {
		if reference.Kind != "link" && reference.Kind != "parent" || !reference.Present {
			continue
		}
		if reference.Peer == nil {
			return nil, db.ErrNotFound
		}
		subject, exists := issues[reference.Subject.IssueUID]
		if !exists || subject.ProjectUID != reference.Subject.ProjectUID || subject.Deleted {
			continue
		}
		peer, exists := issues[reference.Peer.IssueUID]
		if !exists || peer.ProjectUID != reference.Peer.ProjectUID || peer.Deleted {
			continue // exact signed slot remains retained for peer arrival
		}
		relation := reference.Relation
		if reference.Kind == "parent" {
			relation = "parent"
		}
		wanted[scopedNativeEdgeKey(subject.UID, peer.UID, relation)] = scopedNativeEdge{subject.UID, peer.UID, relation}
	}
	return wanted, nil
}

func (s *Store) scopedMaterializeReferencesTx(ctx context.Context, tx *sql.Tx, m db.ScopedMaterialization, issues map[string]db.ScopedIssueState) error {
	wanted, err := scopedMaterializedEdges(m.Collections.References, issues)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.from_issue_uid,l.to_issue_uid,l.type FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN projects fp ON fp.id=f.project_id JOIN issues t ON t.id=l.to_issue_id JOIN projects tp ON tp.id=t.project_id WHERE EXISTS(SELECT 1 FROM federation_scoped_projects o WHERE o.owner_view_uid=$1 AND o.owner_generation=$2 AND o.serving_fenced=0 AND (o.project_uid=fp.uid OR o.project_uid=tp.uid))`, m.Fence.View.Key.ViewUID, m.Fence.View.Key.Generation)
	if err != nil {
		return err
	}
	remove := []int64{}
	for rows.Next() {
		var id int64
		var from, to, relation string
		if err = rows.Scan(&id, &from, &to, &relation); err != nil {
			break
		}
		key := scopedNativeEdgeKey(from, to, relation)
		if _, keep := wanted[key]; keep {
			delete(wanted, key)
		} else {
			remove = append(remove, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range remove {
		var mappings int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_mappings WHERE link_id=$1`, id).Scan(&mappings); err != nil {
			return err
		}
		if mappings != 0 {
			return db.ErrScopedCapturePending
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id=$1`, id); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		edge := wanted[key]
		var from, to int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1`, edge.from).Scan(&from); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1`, edge.to).Scan(&to); err != nil {
			return err
		}
		if edge.relation == "related" && from > to {
			from, to, edge.from, edge.to = to, from, edge.to, edge.from
		}
		// Author/time here are local required storage bookkeeping. Only the
		// retained root-signed relation is verified projected content.
		if _, err := tx.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,created_at) VALUES($1,$2,$3,$4,$5,'system',$6)`, from, to, edge.from, edge.to, edge.relation, issues[edge.from].CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}
