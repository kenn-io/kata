package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type projectAccessContextKey struct{}
type projectHostedContextKey struct{}
type projectTrustedCallerContextKey struct{}

// ProjectAccessDecision records the principal and policy epoch admitted for a
// request. Native transactions recheck every resolved target under the policy
// lock; readers discard a response if its admission epoch changed.
type ProjectAccessDecision struct {
	ProjectUIDs    []string
	PolicyRevision int64
	Actor          string
	targets        []string
	hydratedIssues map[string]bool
	store          db.Storage
	owner          bool
}

func projectOwnerAuthority(ctx context.Context) bool {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		hosted, _ := ctx.Value(projectHostedContextKey{}).(bool)
		trustedCaller, _ := ctx.Value(projectTrustedCallerContextKey{}).(bool)
		return !hosted && (trustedCaller || ownerLocalTransport(ctx)) && !insecureReadonlyRequest(ctx) && !unauthenticatedPrivateNetworkRequest(ctx)
	}
	return p.Kind == PrincipalBootstrap || p.Kind == PrincipalStaticToken || p.Kind == PrincipalWebLocal
}

func projectDiagnosticOwnerAuthority(ctx context.Context) bool {
	principal, authenticated := PrincipalFromContext(ctx)
	return projectOwnerAuthority(ctx) && (!authenticated || principal.Kind == PrincipalBootstrap || principal.Kind == PrincipalStaticToken)
}

func projectAccessDenied() error {
	return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
}

func authorizeProjectTarget(ctx context.Context, projectUID string) error {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || decision.owner {
		return nil
	}
	if !slices.Contains(decision.ProjectUIDs, projectUID) {
		return projectAccessDenied()
	}
	if !slices.Contains(decision.targets, projectUID) {
		decision.targets = append(decision.targets, projectUID)
	}
	return nil
}

func recordProjectAccessHydratedIssue(ctx context.Context, issueUID string, allowDeleted bool) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || decision.owner || issueUID == "" {
		return
	}
	if decision.hydratedIssues == nil {
		decision.hydratedIssues = make(map[string]bool)
	}
	if previous, ok := decision.hydratedIssues[issueUID]; ok {
		decision.hydratedIssues[issueUID] = previous && allowDeleted
		return
	}
	decision.hydratedIssues[issueUID] = allowDeleted
}

func recordProjectAccessMutationRevision(ctx context.Context, revision int64) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || revision <= 0 {
		return
	}
	decision.PolicyRevision = revision
}

func recordProjectAccessCatalogMutation(ctx context.Context, projectUID string) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || decision.owner {
		return
	}
	decision.PolicyRevision++
	if projectUID != "" && !slices.Contains(decision.ProjectUIDs, projectUID) {
		decision.ProjectUIDs = append(decision.ProjectUIDs, projectUID)
	}
}

func revalidateProjectAccessHydratedIssues(ctx context.Context, decision *ProjectAccessDecision) error {
	for issueUID, allowDeleted := range decision.hydratedIssues {
		issue, err := decision.store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		if errors.Is(err, db.ErrNotFound) {
			return projectAccessDenied()
		}
		if err != nil {
			return err
		}
		project, err := decision.store.ProjectByID(ctx, issue.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return projectAccessDenied()
		}
		if err != nil {
			return err
		}
		if (issue.DeletedAt != nil && !allowDeleted) || project.DeletedAt != nil || !slices.Contains(decision.ProjectUIDs, project.UID) {
			return projectAccessDenied()
		}
	}
	return nil
}

func withProjectAuthorization(store db.Storage, hosted, trustedCaller bool, transportRoutes selfAuthenticatedRouteMatcher, noProjectRoutes noProjectDataRouteMatcher, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), projectHostedContextKey{}, hosted))
		r = r.WithContext(context.WithValue(r.Context(), projectTrustedCallerContextKey{}, trustedCaller && !hosted))
		principal, authenticated := PrincipalFromContext(r.Context())
		// Reject malformed host identity before querying native actor policy;
		// the Huma host middleware applies the same authentication contract.
		if hosted && authenticated && !validHostPrincipal(principal) {
			api.WriteEnvelope(w, http.StatusUnauthorized, "authentication_required", "authentication required")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/debug/pprof/") && !projectDiagnosticOwnerAuthority(r.Context()) {
			api.WriteEnvelope(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		owner := projectOwnerAuthority(r.Context())
		if owner && !transportRoutes.matches(r) {
			next.ServeHTTP(w, r)
			return
		}
		if store == nil || (!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/openapi.")) {
			next.ServeHTTP(w, r)
			return
		}
		if noProjectRoutes.matches(r) {
			next.ServeHTTP(w, r)
			return
		}
		decision := &ProjectAccessDecision{Actor: principal.Actor, store: store, owner: owner}
		revision, err := store.ProjectAccessRevision(r.Context())
		if err != nil {
			api.WriteEnvelope(w, 500, "internal", "internal error")
			return
		}
		decision.PolicyRevision = revision
		if owner {
			// Transport handlers resolve their credentials after dispatch, but an
			// owner principal already present here must retain the same unrestricted
			// project selection as every other route.
			decision.ProjectUIDs = nil
		} else if principal.Actor != "" {
			decision.ProjectUIDs, err = store.AccessibleProjectUIDs(r.Context(), principal.Actor)
			if decision.ProjectUIDs == nil {
				decision.ProjectUIDs = []string{}
			}
		} else {
			// Anonymous reads retain only explicitly unrestricted projects. Missing
			// proxy identity never borrows another actor's team memberships.
			decision.ProjectUIDs, err = store.AnonymousAccessibleProjectUIDs(r.Context())
			if decision.ProjectUIDs == nil {
				decision.ProjectUIDs = []string{}
			}
		}
		if err != nil {
			api.WriteEnvelope(w, 500, "internal", "internal error")
			return
		}
		ctx := context.WithValue(r.Context(), projectAccessContextKey{}, decision)
		ctx = db.WithAuthorizedProjects(ctx, decision.ProjectUIDs)
		if unauthenticatedPrivateNetworkRequest(ctx) {
			ctx = db.WithUnrestrictedProjectWrites(ctx)
		}
		if principal.Scope != nil {
			if err := authorizeProjectTarget(ctx, principal.Scope.ProjectUID); err != nil {
				api.WriteEnvelope(w, http.StatusNotFound, "not_found", "resource not found")
				return
			}
		}
		ctx = db.WithAdditionalTransactionFence(ctx, func(fenceCtx context.Context, tx db.Transaction) error {
			if decision.owner || len(decision.targets) == 0 {
				return nil
			}
			err := store.ProjectAccessTransactionFence(decision.Actor, decision.targets)(fenceCtx, tx)
			if errors.Is(err, db.ErrNotFound) {
				return projectAccessDenied()
			}
			return err
		})
		ctx = db.WithProjectAccessTransactionCheck(ctx, func(
			checkCtx context.Context, tx db.Transaction, projectUID string,
		) error {
			if decision.owner {
				return nil
			}
			err := store.ProjectAccessTransactionFence(decision.Actor, []string{projectUID})(checkCtx, tx)
			if errors.Is(err, db.ErrNotFound) {
				return projectAccessDenied()
			}
			return err
		})
		r = r.WithContext(ctx)
		if r.URL.Path == pathEventsStreamPath {
			next.ServeHTTP(w, r)
			return
		}
		response := newBufferedScopedResponse(w)
		defer func() { _ = response.close() }()
		next.ServeHTTP(response, r)
		if err := response.prepare(); err != nil {
			api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		current, err := store.ProjectAccessRevision(ctx)
		if err != nil {
			api.WriteEnvelope(w, 500, "internal", "internal error")
			return
		}
		if current != decision.PolicyRevision {
			api.WriteEnvelope(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if err := revalidateProjectAccessHydratedIssues(ctx, decision); err != nil {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				api.WriteEnvelope(w, http.StatusNotFound, "not_found", "resource not found")
				return
			}
			api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		response.writeTo(w)
	})
}

func withProjectOperationAuthorization(humaAPI huma.API, store db.Storage) {
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		if op == nil {
			writeHostAccessError(ctx, 404, "not_found", "resource not found")
			return
		}
		policy, known := hostOperationPolicy(op.OperationID)
		if !known {
			writeHostAccessError(ctx, 404, "not_found", "resource not found")
			return
		}
		// Enrollment handlers own their narrow transport authentication. Ordinary
		// user credentials still carry this request's project query boundary.
		if _, authenticated := PrincipalFromContext(ctx.Context()); !authenticated && hostAccessRuleFor(op.OperationID).SelfAuthenticated {
			next(ctx)
			return
		}
		// Project selectors and ordinary metadata remain available to members,
		// including credential-bound self-enrollment. Destructive maintenance
		// and project catalog administration require independent owner authority.
		projectAdministration := policy.Kind == hostOperationProjectAdministration && op.OperationID != "resolveProject" && op.OperationID != "initProject" && op.OperationID != "patchProjectMetadata"
		destructiveAdministration := op.OperationID == "purgeIssue" || op.OperationID == "rewriteAuthorIdentity"
		ownerOnly := projectAdministration || destructiveAdministration || policy.Kind == hostOperationTokenAdministration || (policy.Kind == hostOperationIntegrationAdministration && !externalRootOperationIDs[op.OperationID]) || op.OperationID == "getFederationStatus" || op.OperationID == "listFederationEnrollments" || op.OperationID == "revokeFederationEnrollment" || op.OperationID == "createFederationReplica" || op.OperationID == "disconnectFederationBridge" || op.OperationID == "connectFederationBridge" || op.OperationID == "getFederationBridgeStatus"
		principal, authenticated := PrincipalFromContext(ctx.Context())
		// A mounting host adjudicates its administrative capabilities through
		// HostAccess. Native project selection/fences still intersect that grant.
		hosted, _ := ctx.Context().Value(projectHostedContextKey{}).(bool)
		hostPrincipal := hosted && authenticated && validHostPrincipal(principal)
		// Doctor collects no project projection and checks operator identity and
		// transport itself before reading diagnostics. Preserve its owner-local
		// read-only authority and refusal contract.
		if !projectOwnerAuthority(ctx.Context()) && !hostPrincipal && ownerOnly && op.OperationID != "doctor" {
			writeHostAccessError(ctx, 404, "not_found", "resource not found")
			return
		}
		id, valid := positiveProjectID(ctx.Param("project_id"))
		if !valid {
			id, valid = positiveProjectID(ctx.Query("project_id"))
		}
		if valid {
			project, err := store.ProjectByID(ctx.Context(), id)
			if err != nil || authorizeProjectTarget(ctx.Context(), project.UID) != nil {
				writeHostAccessError(ctx, 404, "not_found", "resource not found")
				return
			}
		}
		if uid := ctx.Query("project_uid"); uid != "" {
			if err := authorizeProjectTarget(ctx.Context(), uid); err != nil {
				writeHostAccessError(ctx, 404, "not_found", "resource not found")
				return
			}
		}
		next(ctx)
	})
}
