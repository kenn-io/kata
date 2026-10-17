package daemon

import (
	"context"
	"errors"
	"fmt"
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
// lock; readers discard a response if the caller lost an admitted project or
// an admitted issue moved out of the admitted projects.
type ProjectAccessDecision struct {
	ProjectUIDs    []string
	PolicyRevision int64
	Actor          string
	targets        []string
	store          db.Storage
	owner          bool
	fixedUIDs      []string
	// streamCursor is the last event id an event stream has processed, starting
	// at the high-water mark read before admission. A project whose lifecycle
	// event follows it is new to the stream and still has that event to deliver.
	streamCursor int64
}

// advanceProjectAccessStreamCursor records that the stream has delivered every
// event through id.
func advanceProjectAccessStreamCursor(ctx context.Context, id int64) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision != nil && id > decision.streamCursor {
		decision.streamCursor = id
	}
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

// eventStreamRequest reports the SSE route on the direct path and on the
// daemon switcher's gateway path, where the local target serves it in-process.
func eventStreamRequest(r *http.Request) bool {
	return r.URL.Path == pathEventsStreamPath || r.URL.Path == webDaemonProxyPrefix+pathEventsStreamPath
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
	if decision == nil || decision.owner && decision.fixedUIDs == nil {
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

// recordProjectAccessCatalogMutation admits a project this request created or
// restored so later steps of the same request can address it.
func recordProjectAccessCatalogMutation(ctx context.Context, projectUID string) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || decision.owner || decision.fixedUIDs != nil {
		return
	}
	if projectUID != "" && !slices.Contains(decision.ProjectUIDs, projectUID) {
		decision.ProjectUIDs = append(decision.ProjectUIDs, projectUID)
	}
}

// accessibleProjectUIDs returns the projects actor may select under the
// current policy. Anonymous reads retain only explicitly unrestricted
// projects; missing proxy identity never borrows another actor's teams.
func accessibleProjectUIDs(ctx context.Context, store db.Storage, actor string) ([]string, error) {
	var (
		uids []string
		err  error
	)
	if actor != "" {
		uids, err = store.AccessibleProjectUIDs(ctx, actor)
	} else {
		uids, err = store.AnonymousAccessibleProjectUIDs(db.WithAuthorizedProjects(ctx, nil))
	}
	if err != nil {
		return nil, fmt.Errorf("reading accessible projects: %w", err)
	}
	if uids == nil {
		uids = []string{}
	}
	return uids, nil
}

// refresh re-reads the caller's projects after the policy revision moved. It
// reports kept=false when the caller lost a project admitted for this request;
// otherwise the decision adopts the current set and returns the projects it
// gained, so projects created or restored since admission become selectable.
// The revision is read before the project list so a policy change between the
// two reads surfaces on the next refresh instead of hiding behind the newer
// revision.
func (d *ProjectAccessDecision) refresh(ctx context.Context) (gained []string, kept, changed bool, err error) {
	if d.owner && d.fixedUIDs == nil {
		return nil, true, false, nil
	}
	revision, err := d.store.ProjectAccessRevision(ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("reading project access revision: %w", err)
	}
	if revision == d.PolicyRevision {
		return nil, true, false, nil
	}
	accessible := slices.Clone(d.fixedUIDs)
	if !d.owner {
		accessible, err = accessibleProjectUIDs(ctx, d.store, d.Actor)
		if err != nil {
			return nil, false, true, err
		}
		accessible = narrowProjectUIDs(accessible, d.fixedUIDs)
	}
	for _, projectUID := range d.ProjectUIDs {
		if slices.Contains(accessible, projectUID) {
			continue
		}
		// A project this request merged or purged away is gone for everyone;
		// only an existing project the caller can no longer see is a loss.
		if _, err := d.store.ProjectByUID(ctx, projectUID); errors.Is(err, db.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, false, true, fmt.Errorf("resolving admitted project %s: %w", projectUID, err)
		}
		return nil, false, true, nil
	}
	for _, projectUID := range accessible {
		if !slices.Contains(d.ProjectUIDs, projectUID) {
			gained = append(gained, projectUID)
		}
	}
	d.ProjectUIDs = accessible
	d.PolicyRevision = revision
	return gained, true, true, nil
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
		fixedUIDs, scopeErr := requestProjectScope(r.Context(), store, r.Header)
		if scopeErr != nil {
			api.WriteEnvelope(w, scopeErr.Status, scopeErr.Code, scopeErr.Message)
			return
		}
		owner := projectOwnerAuthority(r.Context())
		if owner && fixedUIDs == nil && !transportRoutes.matches(r) {
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
		decision := &ProjectAccessDecision{Actor: principal.Actor, store: store, owner: owner, fixedUIDs: fixedUIDs}
		var err error
		eventStream := eventStreamRequest(r)
		if eventStream && !owner {
			// Read before the policy so a project created during admission is
			// either admitted now or recognized later as new to the stream.
			decision.streamCursor, err = store.MaxEventID(r.Context())
			if err != nil {
				api.WriteEnvelope(w, 500, "internal", "internal error")
				return
			}
		}
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
		} else {
			decision.ProjectUIDs, err = accessibleProjectUIDs(r.Context(), store, principal.Actor)
		}
		if err != nil {
			api.WriteEnvelope(w, 500, "internal", "internal error")
			return
		}
		decision.ProjectUIDs = narrowProjectUIDs(decision.ProjectUIDs, fixedUIDs)
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
			if decision.fixedUIDs != nil && !slices.Contains(decision.fixedUIDs, projectUID) {
				return projectAccessDenied()
			}
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
		if eventStream {
			// The stream revalidates authority before every frame itself and
			// needs a flushing writer, which the buffered response cannot offer.
			next.ServeHTTP(w, r)
			return
		}
		// Every issue a store reader returns to this request is recorded once
		// here, whichever authorization layer or handler asked for it. A stream
		// rechecks each frame instead, so it keeps no registry for its lifetime.
		ctx = db.WithExposedIssues(ctx)
		r = r.WithContext(ctx)
		response := newBufferedScopedResponse(w)
		defer func() { _ = response.close() }()
		next.ServeHTTP(response, r)
		if err := response.prepare(); err != nil {
			api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		// One rule for every response: the caller must still hold each project
		// admitted for the request, and each issue a store reader returned to
		// it must still sit in one of those projects. A write that committed
		// stays committed even when its response is withheld; unrelated policy
		// and catalog changes leave the response intact.
		if err := decision.revalidateRelease(ctx); err != nil {
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

func (d *ProjectAccessDecision) revalidateRelease(ctx context.Context) error {
	if d.owner && d.fixedUIDs == nil {
		return nil
	}
	_, kept, changed, err := d.refresh(ctx)
	if err != nil {
		return err
	}
	if !kept {
		return projectAccessDenied()
	}
	if !changed {
		return nil
	}
	// Policy changes and issue moves both advance the revision. Re-read each
	// exposed issue without a project filter: one this request purged is gone
	// for everyone, while one that now sits outside the refreshed project set
	// has moved away from the caller.
	unrestricted := db.WithAuthorizedProjects(ctx, nil)
	for _, issueID := range db.ExposedIssues(ctx) {
		issue, err := d.store.IssueByID(unrestricted, issueID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("rechecking exposed issue %d: %w", issueID, err)
		}
		if !slices.Contains(d.ProjectUIDs, issue.ProjectUID) {
			return projectAccessDenied()
		}
	}
	return nil
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
		// Project administration, destructive maintenance and issue sync follow
		// project access; the selectors below fence every project they name.
		// Token, team and federation administration grant or route authority
		// beyond one project, so they stay with the daemon owner.
		ownerOnly := policy.Kind == hostOperationTokenAdministration || op.OperationID == "getFederationStatus" || op.OperationID == "listFederationEnrollments" || op.OperationID == "revokeFederationEnrollment" || op.OperationID == "createFederationReplica" || op.OperationID == "disconnectFederationBridge" || op.OperationID == "connectFederationBridge" || op.OperationID == "getFederationBridgeStatus" || op.OperationID == "repinFederationRootKey"
		principal, authenticated := PrincipalFromContext(ctx.Context())
		// A mounting host adjudicates its administrative capabilities through
		// HostAccess. Native project selection/fences still intersect that grant.
		hosted, _ := ctx.Context().Value(projectHostedContextKey{}).(bool)
		hostPrincipal := hosted && authenticated && validHostPrincipal(principal)
		if !projectOwnerAuthority(ctx.Context()) && !hostPrincipal && ownerOnly {
			writeHostAccessError(ctx, 404, "not_found", "resource not found")
			return
		}
		id, valid := positiveProjectID(ctx.Param("project_id"))
		if !valid {
			id, valid = positiveProjectID(ctx.Query("project_id"))
		}
		if valid {
			project, err := store.ProjectByID(ctx.Context(), id)
			if err != nil {
				writeHostAccessError(ctx, 404, "not_found", "resource not found")
				return
			}
			if authorizeProjectTarget(ctx.Context(), project.UID) != nil {
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

// projectPolicyHidesProjects reports whether project policy or a token scope
// hides any project from the request principal. A principal that sees every
// project carries the same authority it had before project policy existed.
func projectPolicyHidesProjects(ctx context.Context) (bool, error) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil {
		_, restricted := db.AuthorizedProjects(ctx)
		return restricted, nil
	}
	if decision.owner && decision.fixedUIDs == nil {
		return false, nil
	}
	if principal, _ := PrincipalFromContext(ctx); principal.Scope != nil {
		return true, nil
	}
	projects, err := decision.store.ListProjectsIncludingArchived(db.WithAuthorizedProjects(ctx, nil))
	if err != nil {
		return false, fmt.Errorf("list projects for gateway authority: %w", err)
	}
	for _, project := range projects {
		if !slices.Contains(decision.ProjectUIDs, project.UID) {
			return true, nil
		}
	}
	return false, nil
}
