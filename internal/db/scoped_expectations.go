package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
	"slices"
	"strings"
	"time"
)

func ScopedCommandPreparationTx(ctx context.Context, tx *sql.Tx, command ScopedCommand) (ScopedMutationPreparation, error) {
	if err := command.Validate(); err != nil {
		return ScopedMutationPreparation{}, err
	}
	preparation := ScopedMutationPreparation{Kind: command.Kind}
	var issueID int64
	var peers []int64
	switch command.Kind {
	case "closeIssue":
		if command.Close.IfMatchRev != nil {
			return preparation, errors.New("canonical revision guards are unsupported for scoped preparation")
		}
		issueID, preparation.Predicate = command.Close.IssueID, "close"
	case "patchIssueMetadata":
		issueID, preparation.Predicate = command.Metadata.IssueID, "metadata"
		guard := command.Metadata.Guard
		if guard == nil {
			return preparation, errors.New("metadata preparation requires an explicit guard")
		}
		if _, ok := command.Metadata.Patch[guard.Key]; !ok {
			return preparation, errors.New("metadata guard key is not patched")
		}
		preparation.MetadataGuard = &ScopedMetadataPredicate{Key: guard.Key, IfValue: JSONBlob(guard.IfValue), IfAbsent: guard.IfAbsent}
	case "patchProjectMetadata":
		// Native project parameters have no conditional metadata key. Actual
		// callers use the stable DTO entrypoint for that explicit precondition.
		return preparation, errors.New("project metadata guard requires PrepareScopedMutation")
	case "editIssue":
		preparation.Predicate = "parent"
		if command.Edit == nil || command.Edit.SetParent == nil {
			return preparation, errors.New("edit has no conditional parent predicate")
		}
		issueID = command.Edit.IssueID
		peers = append(peers, *command.Edit.SetParent)
	case "replaceCronJob", "archiveCronJob", "restoreCronJob", "replaceCronWorkflow", "archiveCronWorkflow", "restoreCronWorkflow":
		projectID, definition, kind := int64(0), "", "workflow"
		if command.Job != nil {
			projectID, definition, kind = command.Job.ProjectID, command.Job.UID, "job"
		} else {
			projectID, definition = command.Workflow.ProjectID, command.Workflow.UID
		}
		var project string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, projectID).Scan(&project); err != nil {
			return preparation, scopedPreparationLookup(err)
		}
		if command.Subject != (ScopedEndpoint{}) && command.Subject != (ScopedEndpoint{ProjectUID: project}) {
			return preparation, ErrNotFound
		}
		preparation.Subject = ScopedMutationSubject{Kind: kind, ProjectUID: project, DefinitionUID: definition}
		preparation.Predicate = "definition"
	default:
		return preparation, errors.New("command has no conditional scoped predicate")
	}
	if issueID != 0 {
		endpoint, err := scopedPreparationIssueIDTx(ctx, tx, issueID)
		if err != nil {
			return preparation, err
		}
		preparation.Subject = ScopedMutationSubject{Kind: "issue", ProjectUID: endpoint.ProjectUID, IssueUID: endpoint.IssueUID}
		if command.Close != nil && command.Close.ExpectedProjectID != 0 {
			var actualProjectID int64
			if err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id=$1`, issueID).Scan(&actualProjectID); err != nil {
				return preparation, scopedPreparationLookup(err)
			}
			if actualProjectID != command.Close.ExpectedProjectID {
				return preparation, ErrIssueProjectChanged
			}
		}
		if command.Subject != (ScopedEndpoint{}) && command.Subject != endpoint {
			return preparation, ErrNotFound
		}
	}
	for _, id := range peers {
		endpoint, err := scopedPreparationIssueIDTx(ctx, tx, id)
		if err != nil {
			return preparation, err
		}
		if !slices.Contains(preparation.Peers, endpoint) {
			preparation.Peers = append(preparation.Peers, endpoint)
		}
	}
	return preparation, preparation.Validate()
}

func scopedPreparationLookup(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func scopedPreparationIssueIDTx(ctx context.Context, tx *sql.Tx, id int64) (ScopedEndpoint, error) {
	var endpoint ScopedEndpoint
	err := tx.QueryRowContext(ctx, `SELECT p.uid,i.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=$1 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, id).Scan(&endpoint.ProjectUID, &endpoint.IssueUID)
	return endpoint, scopedPreparationLookup(err)
}

func ScopedPredicateCurrentTx(ctx context.Context, tx Transaction, project, subject, kind string) (string, error) {
	var raw JSONBlob
	var deleted, projectDeleted bool
	var parent *string
	switch kind {
	case "issue.close":
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT i.revision,i.deleted_at IS NOT NULL,p.deleted_at IS NOT NULL FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2`, subject, project).Scan(&revision, &deleted, &projectDeleted); err != nil {
			return "", scopedPreparationLookup(err)
		}
		return ScopedClosePredicateDigest(revision, deleted, projectDeleted)
	case "project.metadata":
		if project != subject {
			return "", ErrNotFound
		}
		if err := tx.QueryRowContext(ctx, `SELECT metadata,deleted_at IS NOT NULL FROM projects WHERE uid=$1`, project).Scan(&raw, &deleted); err != nil {
			return "", scopedPreparationLookup(err)
		}
	case "issue.metadata", "issue.parent":
		var id, revision int64
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT i.id,i.metadata,i.revision,i.status,i.deleted_at IS NOT NULL,p.deleted_at IS NOT NULL FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2`, subject, project).Scan(&id, &raw, &revision, &status, &deleted, &projectDeleted); err != nil {
			return "", scopedPreparationLookup(err)
		}
		if kind == "issue.parent" {
			err := tx.QueryRowContext(ctx, `SELECT peer.uid FROM links l JOIN issues peer ON peer.id=l.to_issue_id JOIN projects p ON p.id=peer.project_id WHERE l.from_issue_id=$1 AND l.type='parent' AND peer.deleted_at IS NULL AND p.deleted_at IS NULL`, id).Scan(&parent)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
		}
	case "job.definition", "workflow.definition":
		var current CronDefinition
		var tombstone bool
		table := cronTable(kind == "job.definition")
		if err := tx.QueryRowContext(ctx, `SELECT d.uid,d.definition_event_uid,d.deleted_at IS NOT NULL,p.deleted_at IS NOT NULL FROM `+table+` d JOIN projects p ON p.id=d.project_id WHERE d.uid=$1 AND p.uid=$2`, subject, project).Scan(&current.UID, &current.DefinitionEventUID, &tombstone, &projectDeleted); err != nil {
			return "", scopedPreparationLookup(err)
		}
		if tombstone {
			current.DeletedAt = new(time.Time)
		}
		return ScopedDefinitionPredicateDigest(kind, current, projectDeleted)
	default:
		return "", errors.New("invalid scoped predicate storage kind")
	}
	return scopedPredicateStateDigest(project, subject, kind, raw, deleted, projectDeleted, parent)
}

func scopedPredicateStateDigest(project, subject, kind string, rawMetadata JSONBlob, deleted, projectDeleted bool, parent *string) (string, error) {
	var value any
	switch kind {
	case "project.metadata":
		if project != subject {
			return "", ErrNotFound
		}
		value = struct {
			Metadata JSONBlob
			Deleted  bool
		}{JSONBlob(metadata.NormalizeJSON(jsontext.Value(rawMetadata))), deleted}
	case "issue.metadata":
		value = struct {
			Metadata                JSONBlob
			Deleted, ProjectDeleted bool
		}{JSONBlob(metadata.NormalizeJSON(jsontext.Value(rawMetadata))), deleted, projectDeleted}
	case "issue.parent":
		value = struct {
			Parent                  *string
			Deleted, ProjectDeleted bool
		}{parent, deleted, projectDeleted}
	default:
		return "", errors.New("invalid scoped predicate storage kind")
	}
	return scopedPredicateValueDigest(kind, value)
}

func ScopedDefinitionPredicateDigest(kind string, current CronDefinition, projectDeleted bool) (string, error) {
	if kind != "job.definition" && kind != "workflow.definition" || !uid.Valid(current.DefinitionEventUID) {
		return "", errors.New("invalid native definition predicate")
	}
	return scopedPredicateValueDigest(kind, struct {
		DefinitionEventUID      string
		Deleted, ProjectDeleted bool
	}{current.DefinitionEventUID, current.DeletedAt != nil, projectDeleted})
}

func ScopedClosePredicateDigest(revision int64, deleted, projectDeleted bool) (string, error) {
	return scopedPredicateValueDigest("issue.close", struct {
		Revision                int64
		Deleted, ProjectDeleted bool
	}{revision, deleted, projectDeleted})
}

func scopedPredicateValueDigest(kind string, value any) (string, error) {
	raw, err := json.Marshal(struct {
		Domain, Kind string
		Current      any
	}{"kata.scoped.root-predicate.v1", kind, value})
	if err != nil {
		return "", err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return "", err
	}
	return ScopedOfferedDigest(canonical), nil
}

func ValidateScopedPreparationAccessTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, preparation ScopedMutationPreparation, postgres bool) error {
	return validateScopedPreparationAccessTx(ctx, tx, view, preparation, postgres)
}

func validateScopedPreparationAccessTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, preparation ScopedMutationPreparation, postgres bool) error {
	preparation.Peers = slices.Clone(preparation.Peers)
	if preparation.MetadataGuard != nil {
		copy := *preparation.MetadataGuard
		preparation.MetadataGuard = &copy
	}
	if err := preparation.Validate(); err != nil {
		return err
	}
	selected := func(project string) bool {
		return slices.ContainsFunc(view.Projects, func(p ScopedProjectPin) bool { return p.ProjectUID == project })
	}
	checkProject := func(project string) error {
		if !selected(project) {
			return ErrNotFound
		}
		if allowed, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(allowed, project) {
			return ErrNotFound
		}
		return CheckProjectAccessTransaction(ctx, tx, project)
	}
	if err := checkProject(preparation.Subject.ProjectUID); err != nil {
		return err
	}
	lock := ""
	if postgres {
		lock = " FOR UPDATE"
	}
	var projectID int64
	var projectMetadata JSONBlob
	if err := tx.QueryRowContext(ctx, `SELECT id,metadata FROM projects WHERE uid=$1 AND deleted_at IS NULL`+lock, preparation.Subject.ProjectUID).Scan(&projectID, &projectMetadata); err != nil {
		return scopedPreparationLookup(err)
	}
	endpoints := slices.Clone(preparation.Peers)
	if preparation.Predicate == "definition" {
		var definition *cron.JobDefinition
		// Root/canonical preparation always reads the actual native row and
		// codec under the SAME locks; local signed data never enters it.
		table := cronTable(preparation.Subject.Kind == "job")
		current, err := scanCron(tx.QueryRowContext(ctx, `SELECT `+cronDefinitionColumns+` FROM `+table+` WHERE uid=$1 AND project_id=$2`+lock, preparation.Subject.DefinitionUID, projectID))
		if err != nil {
			return err
		}
		if preparation.Subject.Kind == "job" {
			job, err := jobFromRow(current)
			if err != nil {
				return err
			}
			definition = &job.Definition
		}
		if definition != nil {
			refs := []string{definition.Trigger.IssueUID}
			if definition.Issue != nil {
				refs = append(refs, definition.Issue.UID, definition.Issue.ParentUID)
			}
			for _, ref := range refs {
				peer := ScopedEndpoint{ProjectUID: preparation.Subject.ProjectUID, IssueUID: ref}
				if ref != "" && !slices.Contains(endpoints, peer) {
					endpoints = append(endpoints, peer)
				}
			}
			if ref := definition.Action.WorkflowUID; ref != "" {
				var id int64
				if err := tx.QueryRowContext(ctx, `SELECT id FROM cron_workflows WHERE uid=$1 AND project_id=$2`+lock, ref, projectID).Scan(&id); err != nil {
					return scopedPreparationLookup(err)
				}
			}
		}
	}
	if preparation.Subject.Kind == "issue" {
		endpoints = append(endpoints, ScopedEndpoint{ProjectUID: preparation.Subject.ProjectUID, IssueUID: preparation.Subject.IssueUID})
	}
	if preparation.Predicate == "parent" {
		// Caller peers cannot omit the actual parent from native authorization.
		// Resolve it in the same transaction that reads the guarded current state.
		var current ScopedEndpoint
		err := tx.QueryRowContext(ctx, `SELECT p.uid,peer.uid FROM issues child JOIN projects cp ON cp.id=child.project_id JOIN links l ON l.from_issue_id=child.id AND l.type='parent' JOIN issues peer ON peer.id=l.to_issue_id JOIN projects p ON p.id=peer.project_id WHERE child.uid=$1 AND cp.uid=$2 AND child.deleted_at IS NULL AND peer.deleted_at IS NULL AND p.deleted_at IS NULL`, preparation.Subject.IssueUID, preparation.Subject.ProjectUID).Scan(&current.ProjectUID, &current.IssueUID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return scopedPreparationLookup(err)
		}
		if err == nil && !slices.Contains(endpoints, current) {
			endpoints = append(endpoints, current)
		}
	}
	slices.SortFunc(endpoints, func(a, b ScopedEndpoint) int { return strings.Compare(a.IssueUID, b.IssueUID) })
	var issueMetadata JSONBlob
	for _, endpoint := range endpoints {
		if err := checkProject(endpoint.ProjectUID); err != nil {
			return err
		}
		var id int64
		var raw JSONBlob
		issueLock := ""
		if postgres {
			issueLock = " FOR UPDATE OF i"
		}
		if err := tx.QueryRowContext(ctx, `SELECT i.id,i.metadata FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`+issueLock, endpoint.IssueUID, endpoint.ProjectUID).Scan(&id, &raw); err != nil {
			return scopedPreparationLookup(err)
		}
		RecordIssueScopeTarget(ctx, id)
		if endpoint.IssueUID == preparation.Subject.IssueUID {
			issueMetadata = raw
		}
	}
	if preparation.Predicate == "metadata" {
		current := issueMetadata
		if preparation.Subject.Kind == "project" {
			current = projectMetadata
		}
		guard := preparation.MetadataGuard
		// Preparation explicitly names this key. The existing native checker
		// performs its value/absence/null/normalization comparison unchanged.
		if err := CheckMetadataPatchGuard(jsontext.Value(current), map[string]jsontext.Value{guard.Key: jsontext.Value(guard.IfValue)}, &MetadataPatchGuard{Key: guard.Key, IfValue: jsontext.Value(guard.IfValue), IfAbsent: guard.IfAbsent}); err != nil {
			return err
		}
	}
	// Apply the independently supplied native caller/token/issue-scope fence
	// only after every actual subject/peer has been recorded, before any write.
	if err := ApplyTransactionFence(ctx, tx); err != nil {
		return err
	}
	return nil
}

func PrepareScopedExpectationTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, principal CustodyPrincipal, preparation ScopedMutationPreparation, command *ScopedCommand, postgres bool) (MutationExpectation, error) {
	preparation.Peers = slices.Clone(preparation.Peers)
	if preparation.MetadataGuard != nil {
		copy := *preparation.MetadataGuard
		preparation.MetadataGuard = &copy
	}
	if err := preparation.Validate(); err != nil {
		return MutationExpectation{}, err
	}
	if principal.Validate() != nil || principal.AuthorityUID != view.AuthorityUID || principal.RouteDigest != view.RouteDigest {
		return MutationExpectation{}, ErrNotFound
	}
	if err := ValidateScopedPreparationAccessTx(ctx, tx, view, preparation, postgres); err != nil {
		return MutationExpectation{}, err
	}
	lock := ""
	if postgres {
		lock = " FOR UPDATE"
	}
	kind, err := ScopedPredicateStorageKind(preparation)
	if err != nil {
		return MutationExpectation{}, err
	}
	subject := scopedSubject(preparation)
	digest, err := ScopedPredicateCurrentTx(ctx, tx, preparation.Subject.ProjectUID, subject, kind)
	if err != nil {
		return MutationExpectation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_predicate_incarnations(project_uid,subject_uid,predicate_kind,incarnation,canonical_digest) VALUES($1,$2,$3,1,$4) ON CONFLICT(project_uid,subject_uid,predicate_kind) DO NOTHING`, preparation.Subject.ProjectUID, subject, kind, digest); err != nil {
		return MutationExpectation{}, err
	}
	var incarnation int64
	var retainedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT incarnation,canonical_digest FROM federation_predicate_incarnations WHERE project_uid=$1 AND subject_uid=$2 AND predicate_kind=$3`+lock, preparation.Subject.ProjectUID, subject, kind).Scan(&incarnation, &retainedDigest); err != nil {
		return MutationExpectation{}, err
	}
	if retainedDigest != digest {
		return MutationExpectation{}, ErrScopedSourceConflict
	}
	predicate, err := json.Marshal(ScopedMutationPredicate{Version: 1, Preparation: preparation, Incarnation: incarnation, CanonicalDigest: digest})
	if err != nil {
		return MutationExpectation{}, err
	}
	principalBytes, err := json.Marshal(principal)
	if err != nil {
		return MutationExpectation{}, err
	}
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return MutationExpectation{}, err
	}
	token := MutationExpectation{Token: base64.RawURLEncoding.EncodeToString(entropy[:32])}
	row := ScopedMutationExpectationExport{TokenDigest: ScopedOfferedDigest([]byte(token.Token)), ProjectUID: preparation.Subject.ProjectUID, SubjectUID: subject, PredicateKind: kind, PredicateBytes: predicate, PrincipalBytes: principalBytes, RouteDigest: principal.RouteDigest, Nonce: base64.RawURLEncoding.EncodeToString(entropy[32:]), ExpiresAt: time.Now().UTC().Add(ScopedPreparationLifetimeSeconds * time.Second).Format(time.RFC3339Nano)}
	if err := ReplayScopedRecord(ctx, tx, &row); err != nil {
		return MutationExpectation{}, err
	}
	if err := ScopedSourceBudgetTx(ctx, tx); err != nil {
		return MutationExpectation{}, err
	}
	return token, nil
}
