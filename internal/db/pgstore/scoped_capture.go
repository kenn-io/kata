package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"sort"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Native current-state helpers also compose with SQLite's manual purge
// transaction connection. Both forms retain one actual committing SQL cut.
type scopedCanonicalTx interface {
	db.Transaction
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// CaptureScopedView retains one immutable private canonical cut. It does not
// activate, publish, sign root facts or promote incoming normalized replicas.
func (s *Store) CaptureScopedView(ctx context.Context, key db.ScopedViewKey) (db.CanonicalViewState, error) {
	var result db.CanonicalViewState
	if err := key.Validate(); err != nil {
		return result, err
	}
	err := s.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.CanonicalViewState{}
		view, err := s.fenceScopedAuthorityTx(ctx, tx, key, "pull")
		if err != nil {
			return err
		}
		if view.AuthorityUID != s.instanceUID {
			return db.ErrNotFound
		}
		var work db.ScopedWorkExport
		err = tx.QueryRowContext(ctx, `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work WHERE view_uid=$1`+` FOR UPDATE`, key.ViewUID).Scan(&work.ViewUID, &work.DirtyRevision, &work.CapturedRevision, &work.CaptureUID, &work.CaptureBytes, &work.CaptureDigest, &work.State)
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		captured := work.CaptureUID != nil || work.CaptureBytes != nil || work.CaptureDigest != nil || work.CapturedRevision != nil
		if captured {
			if work.CaptureUID == nil || work.CaptureBytes == nil || work.CaptureDigest == nil || work.CapturedRevision == nil || *work.CapturedRevision > work.DirtyRevision || work.State != "captured" || db.ScopedOfferedDigest(work.CaptureBytes) != *work.CaptureDigest {
				return errors.New("invalid retained canonical capture tuple")
			}
			state, err := db.DecodeScopedCanonicalCapture(work.CaptureBytes)
			if err != nil {
				return err
			}
			if state.CaptureUID != *work.CaptureUID {
				return errors.New("retained capture identity differs")
			}
			if len(state.ProjectCuts) != len(view.Projects) {
				return db.ErrScopedCapturePending
			}
			for _, p := range view.Projects {
				if _, ok := state.ProjectCuts[p.ProjectUID]; !ok {
					return db.ErrScopedCapturePending
				}
			}
			result = state
			return nil
		}
		if work.State != "idle" && work.State != "dirty" {
			return errors.New("invalid uncaptured canonical work state")
		}
		state, err := s.scopedCanonicalCurrentTx(ctx, tx, view)
		if err != nil {
			return err
		}
		state.CaptureUID, err = uid.New()
		if err != nil {
			return err
		}
		raw, err := db.EncodeScopedCanonicalCapture(state)
		if err != nil {
			return err
		}
		digest := db.ScopedOfferedDigest(raw)
		revision := work.DirtyRevision
		candidate := db.ScopedWorkExport{ViewUID: key.ViewUID, DirtyRevision: revision, CapturedRevision: &revision, CaptureUID: &state.CaptureUID, CaptureBytes: raw, CaptureDigest: &digest, State: "captured"}
		if err := scopedCaptureBudgetTx(ctx, tx, view.Key.ViewUID, candidate); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE federation_scoped_work SET captured_revision=$1,capture_uid=$2,capture_bytes=$3,capture_digest=$4,state='captured' WHERE view_uid=$5 AND capture_uid IS NULL AND dirty_revision=$1`, revision, state.CaptureUID, raw, digest, key.ViewUID)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return db.ErrRemoteEventConflict
		}
		result, err = db.DecodeScopedCanonicalCapture(raw)
		return err
	})
	if err != nil {
		return db.CanonicalViewState{}, err
	}
	return result, nil
}

// All readers use the supplied native transaction and current rows. Edge clocks
// absent from native current SQL remain unknown; no historical clock is guessed.
func (s *Store) scopedCanonicalCurrentTx(ctx context.Context, tx *sql.Tx, view db.ScopedViewDescriptor) (db.CanonicalViewState, error) {
	state := db.CanonicalViewState{ProjectCuts: map[string]int64{}, Projects: []db.Project{}, Creation: []db.ScopedProvenance{}, Close: []db.CanonicalCloseState{}}
	state.Fold = db.FoldEvents(nil) // Initializes empty maps; no source history is folded.
	for _, selected := range view.Projects {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE uid=$1 AND deleted_at IS NULL`, selected.ProjectUID))
		if err != nil {
			return state, err
		}
		// The live authority helper already requires canonical/unowned root rows.
		var cut int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, project.ID).Scan(&cut); err != nil {
			return state, err
		}
		state.ProjectCuts[project.UID] = cut
		state.Projects = append(state.Projects, project)
		state.Fold.ProjectMetadata[project.UID] = jsontext.Value(project.Metadata)
		state.Creation = append(state.Creation, db.ScopedCaptureCreation(project.UID, "project", project.UID, "", "", db.AttributionView{}))
		issues, err := federationIssuesForSnapshot(ctx, tx, project.ID)
		if err != nil {
			return state, err
		}
		for _, issue := range issues {
			proof, err := readScopedCanonicalCloseTx(ctx, tx, issue.UID)
			if err != nil {
				return state, err
			}
			if err := db.ValidateScopedCanonicalCloseState(proof, issue.UID, issue.Status); err != nil {
				return state, err
			}
			current := db.FoldIssue{UID: issue.UID, ProjectUID: project.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Author: issue.Author, Owner: issue.Owner, AssignmentExpiresOn: db.ScopedCaptureTime(issue.AssignmentExpiresOn), Priority: issue.Priority, Status: issue.Status, ClosedReason: issue.ClosedReason, ClosedAt: db.ScopedCaptureTime(issue.ClosedAt), DeletedAt: db.ScopedCaptureTime(issue.DeletedAt), CreatedAt: issue.CreatedAt.UTC().Format(db.EventTimestampFormat), UpdatedAt: issue.UpdatedAt.UTC().Format(db.EventTimestampFormat), StatusIntentUID: proof.StatusIntentUID, StatusClock: proof.StatusClock, CloseBoundaryClock: proof.CloseBoundaryClock, CloseValue: proof.Value}
			state.Fold.Issues[issue.UID] = current
			state.Fold.IssueMetadata[issue.UID] = jsontext.Value(issue.Metadata)
			state.Close = append(state.Close, proof)
			state.Creation = append(state.Creation, db.ScopedCaptureCreation(project.UID, "issue", issue.UID, issue.Author, "", issue.AttributionView))
			labels, err := federationIssueLabels(ctx, tx, issue.ID)
			if err != nil {
				return state, err
			}
			for _, label := range labels {
				state.Fold.Labels[db.FoldLabelKey{IssueUID: issue.UID, Label: label}] = db.FoldElementState{Present: true}
			}
			links, err := federationIssueLinks(ctx, tx, issue.ID)
			if err != nil {
				return state, err
			}
			for _, link := range links {
				from, to := issue.UID, link.ToIssueUID
				if link.Incoming {
					from, to = to, from
				}
				if link.Type == "related" && from > to {
					from, to = to, from
				}
				state.Fold.Links[db.FoldLinkKey{FromUID: from, ToUID: to, Type: link.Type}] = db.FoldElementState{Present: true, Author: link.Author, CreatedAt: link.CreatedAt}
			}
			rows, err := tx.QueryContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE issue_id=$1 ORDER BY id`, issue.ID)
			if err != nil {
				return state, err
			}
			for rows.Next() {
				comment, readErr := scanComment(rows)
				if readErr != nil {
					err = readErr
					break
				}
				edited := ""
				if comment.EditedAt != nil {
					edited = comment.EditedAt.UTC().Format(db.EventTimestampFormat)
				}
				state.Fold.Comments[comment.UID] = db.FoldComment{UID: comment.UID, IssueUID: issue.UID, Author: comment.Author, Teammate: comment.Teammate, Body: comment.Body, CreatedAt: comment.CreatedAt.UTC().Format(time.RFC3339Nano), ReplyToUID: comment.ReplyToUID, ReplyKind: comment.ReplyKind, EditedAt: edited}
				state.Creation = append(state.Creation, db.ScopedCaptureCreation(project.UID, "comment", comment.UID, comment.Author, comment.Teammate, comment.AttributionView))
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				return state, err
			}
		}
		cron := db.CronSQL{Query: tx}
		jobs, err := cron.Jobs(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
		if err != nil {
			return state, err
		}
		for _, job := range jobs {
			state.Fold.CronJobs[job.UID] = db.FoldCronJob{CronJob: job, ProjectUID: project.UID}
			state.Creation = append(state.Creation, db.ScopedCaptureCreation(project.UID, "cron_job", job.UID, job.Author, "", db.AttributionView{}))
		}
		workflows, err := cron.Workflows(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
		if err != nil {
			return state, err
		}
		for _, workflow := range workflows {
			state.Fold.CronWorkflows[workflow.UID] = db.FoldCronWorkflow{CronWorkflow: workflow, ProjectUID: project.UID}
			state.Creation = append(state.Creation, db.ScopedCaptureCreation(project.UID, "cron_workflow", workflow.UID, workflow.Author, "", db.AttributionView{}))
		}
		// Run observations remain an explicit empty private-history family.
		artifacts, err := relayCheckpointArtifactsTx(ctx, tx, project)
		if err != nil {
			return state, err
		}
		state.Artifacts = append(state.Artifacts, artifacts...)
	}
	return state, nil
}

// Admission uses the same exact retained-byte accountant as owner restore.
// Replacing this view's work does not evict any immutable owed representation.
func scopedCaptureBudgetTx(ctx context.Context, tx scopedCanonicalTx, viewUID string, candidate db.ScopedWorkExport) error {
	var total, private int64
	operationBytes := map[string]int64{}
	principals := map[string]db.CustodyPrincipal{}
	// The retained view may index any authenticated included hop. Match the
	// owner accountant's exact indexed tuple, not this issuer's adjacent hop.
	var bindingUID, routeDigest string
	if err := tx.QueryRowContext(ctx, `SELECT binding_uid,route_digest FROM federation_scoped_views WHERE view_uid=$1`, viewUID).Scan(&bindingUID, &routeDigest); err != nil {
		return scopedAuthorityLookupError(err)
	}
	sameRoute := func(p db.CustodyPrincipal) bool { return p.BindingUID == bindingUID && p.RouteDigest == routeDigest }
	for record, err := range db.ExportScopedStateSQL(ctx, tx.QueryContext) {
		if err != nil {
			return err
		}
		if work, ok := record.(*db.ScopedWorkExport); ok && work.ViewUID == candidate.ViewUID {
			record = &candidate
		}
		size := db.ScopedRetainedBytes(record)
		total += size
		switch r := record.(type) {
		case *db.ScopedWorkExport:
			if r.ViewUID == candidate.ViewUID {
				private += size
			}
		case *db.ScopedSourceOperationExport:
			var p db.CustodyPrincipal
			if err := db.DecodeScopedRecord(r.PrincipalBytes, &p); err != nil {
				return err
			}
			principals[r.OperationUID] = p
			operationBytes[r.OperationUID] += size
		case *db.ScopedPrivateSourceExport:
			if r.OperationUID != nil {
				operationBytes[*r.OperationUID] += size
			}
		case *db.ScopedControlCustodyExport:
			var p db.CustodyPrincipal
			if err := db.DecodeScopedRecord(r.PrincipalBytes, &p); err != nil {
				return err
			}
			if sameRoute(p) {
				private += size
			}
		}
	}
	for operation, size := range operationBytes {
		if sameRoute(principals[operation]) {
			private += size
		}
	}
	if total > db.MaxScopedDaemonBytes || private > db.MaxScopedRecoveryBytes {
		return errors.New("scoped canonical capture retained-byte quota exceeded")
	}
	return nil
}

// markScopedCanonicalDirtyTx records private work intent in the same native
// transaction as a current canonical mutation. It never changes an owed tuple
// and never promotes an incoming projection into a root fact.
func (s *Store) markScopedCanonicalDirtyTx(ctx context.Context, tx scopedCanonicalTx, projects ...string) error {
	return markScopedCanonicalDirtyTx(ctx, tx, projects...)
}

// Static projection cleanup uses this same transaction-local implementation.
func markScopedCanonicalDirtyTx(ctx context.Context, tx scopedCanonicalTx, projects ...string) error {
	changed := map[string]bool{}
	for _, project := range projects {
		if project == "" {
			continue
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects p LEFT JOIN federation_scoped_projects sp ON sp.project_uid=p.uid WHERE p.uid=$1 AND sp.owner_view_uid IS NULL`, project).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			changed[project] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	var instanceUID string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instanceUID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.view_uid,g.descriptor_bytes FROM federation_scoped_generations g JOIN federation_scoped_views v ON v.view_uid=g.view_uid JOIN federation_scoped_work w ON w.view_uid=v.view_uid WHERE v.authority_uid=$1 AND v.state IN ('active','prepared')`, instanceUID)
	if err != nil {
		return err
	}
	views := map[string]bool{}
	for rows.Next() {
		var viewUID string
		var raw []byte
		if err = rows.Scan(&viewUID, &raw); err != nil {
			break
		}
		var descriptor db.ScopedViewDescriptor
		if err = db.DecodeScopedRecord(raw, &descriptor); err != nil {
			break
		}
		for _, selected := range descriptor.Projects {
			if changed[selected.ProjectUID] {
				views[viewUID] = true
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	viewUIDs := make([]string, 0, len(views))
	for viewUID := range views {
		viewUIDs = append(viewUIDs, viewUID)
	}
	sort.Strings(viewUIDs) // Shared views always acquire work-row locks in UID order.
	for _, viewUID := range viewUIDs {
		// Only idle->dirty increases retained bytes (one state byte). Numeric
		// revisions have fixed accountant width; dirty/captured retries do not
		// rescan the retained namespace. Lock each work row in sorted order.
		candidate := db.ScopedWorkExport{ViewUID: viewUID}
		if err := tx.QueryRowContext(ctx, `SELECT dirty_revision,state FROM federation_scoped_work WHERE view_uid=$1 FOR UPDATE`, viewUID).Scan(&candidate.DirtyRevision, &candidate.State); err != nil {
			return err
		}
		if candidate.State == "idle" {
			candidate.DirtyRevision++
			candidate.State = "dirty"
			if err := scopedCaptureBudgetTx(ctx, tx, viewUID, candidate); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_work SET dirty_revision=dirty_revision+1,state=CASE WHEN capture_uid IS NULL THEN 'dirty' ELSE 'captured' END WHERE view_uid=$1`, viewUID); err != nil {
			return err
		}
	}
	return nil
}

// completeScopedCaptureWorkTx is only the exact private work CAS. Its caller
// must first complete publication/obligation settlement under the owning native
// locks. Retained conversion inventory still pins the exact bytes, including
// settled inventory until its owner removes it atomically. This helper does not
// publish, activate, acknowledge or release custody.
func completeScopedCaptureWorkTx(ctx context.Context, tx db.Transaction, viewUID, captureUID string, capturedRevision int64) (bool, error) {
	if !uid.Valid(viewUID) || !uid.Valid(captureUID) || capturedRevision < 0 {
		return false, errors.New("invalid capture completion identity")
	}
	result, err := tx.ExecContext(ctx, `UPDATE federation_scoped_work SET state=CASE WHEN dirty_revision>captured_revision THEN 'dirty' ELSE 'idle' END,captured_revision=NULL,capture_uid=NULL,capture_bytes=NULL,capture_digest=NULL WHERE view_uid=$1 AND capture_uid=$2 AND captured_revision=$3 AND NOT EXISTS(SELECT 1 FROM federation_conversion_obligations o WHERE o.kind='capture' AND o.object_uid=$2)`, viewUID, captureUID, capturedRevision)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// markScopedCanonicalEventsDirtyTx runs only after native current mutation.
// Merely retaining source/custody bytes must not call it. Endpoint lookups are
// read-only and never create placeholders or infer hidden project ownership.
func (s *Store) markScopedCanonicalEventsDirtyTx(ctx context.Context, tx scopedCanonicalTx, events ...db.RemoteEvent) error {
	var projects []string
	for _, event := range events {
		class, known := db.ScopedEventClass(event.Type)
		if !known || class == "history" {
			continue
		}
		projects = append(projects, event.ProjectUID)
		if !db.FederationEventAffectsLinks(event.Type) {
			continue
		}
		refs, err := payloadReferencedIssueUIDs(event, db.PayloadMap(event.Payload))
		if err != nil {
			return err
		}
		for _, ref := range refs {
			var project string
			err := tx.QueryRowContext(ctx, `SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, ref).Scan(&project)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			projects = append(projects, project)
		}
	}
	return s.markScopedCanonicalDirtyTx(ctx, tx, projects...)
}
