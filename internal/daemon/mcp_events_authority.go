package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

// mcpEventsPrincipal converts an authenticated request principal to the
// principal that owns an MCP Events subscription (design spec §5.1). Bootstrap
// and every other kind that cannot be re-acquired later are Forbidden.
func mcpEventsPrincipal(p Principal) (mcpevents.Principal, error) {
	switch p.Kind {
	case PrincipalDBToken:
		if p.TokenID <= 0 {
			return mcpevents.Principal{}, mcpevents.ErrForbidden
		}
		return mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: strconv.FormatInt(p.TokenID, 10), Actor: p.Actor}, nil
	case PrincipalStaticToken:
		if p.Subject == "" {
			return mcpevents.Principal{}, mcpevents.ErrForbidden
		}
		return mcpevents.Principal{Authority: mcpevents.AuthorityStaticToken, Subject: p.Subject}, nil
	case PrincipalHost:
		if !validHostPrincipal(p) {
			return mcpevents.Principal{}, mcpevents.ErrForbidden
		}
		return mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: p.Subject, Actor: p.Actor}, nil
	default:
		return mcpevents.Principal{}, mcpevents.ErrForbidden
	}
}

const mcpEventsLocalSubject = "local"

// staticTokenSubject binds durable subscriptions to the authenticating credential
// without storing that credential. Replacing it revokes the old subscriptions.
func staticTokenSubject(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

// MCPEventsPrincipalFromContext derives the subscription owner from the
// request context. A request with no principal is the owner-local identity
// only when the daemon already treats it as owner authority.
func MCPEventsPrincipalFromContext(ctx context.Context) (mcpevents.Principal, error) {
	principal, ok := PrincipalFromContext(ctx)
	if ok {
		return mcpEventsPrincipal(principal)
	}
	if !projectOwnerAuthority(ctx) {
		return mcpevents.Principal{}, mcpevents.ErrForbidden
	}
	return mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: mcpEventsLocalSubject}, nil
}

// mcpEventsAuthority implements mcpevents.Authority from the daemon's own
// authorization decisions. It never makes an HTTP request; it rebuilds the
// context the HTTP middleware would have built and calls the same functions.
type mcpEventsAuthority struct {
	store           db.Storage
	token           string
	requireIdentity bool
	host            HostAccessController
	now             func() time.Time
}

// NewMCPEventsAuthority returns the Authority for a daemon or embedded
// Service. The configuration it reads is the one that decides how requests
// authenticate, so a later change of auth mode is seen at the next
// re-acquisition.
func NewMCPEventsAuthority(cfg ServerConfig) mcpevents.Authority {
	return &mcpEventsAuthority{
		store: cfg.DB, token: cfg.Auth.Token, requireIdentity: cfg.Auth.RequireTokenIdentity,
		host: cfg.HostAccess, now: time.Now,
	}
}

// Reacquire rebuilds the authorization context of a persisted principal.
// ErrPrincipalRevoked means the principal no longer holds authority;
// ErrAuthorityUnavailable means no decision can be made now. A missing piece
// of context is never an "allow".
func (a *mcpEventsAuthority) Reacquire(ctx context.Context, p mcpevents.Principal) (mcpevents.Authorized, error) {
	if a == nil || a.store == nil {
		return nil, mcpevents.ErrAuthorityUnavailable
	}
	var (
		principal *Principal
		hostGrant *HostAccessDecision
	)
	switch p.Authority {
	case mcpevents.AuthorityDBToken:
		// Database tokens authenticate only in identity mode. Outside it the
		// credential no longer resolves, the counterpart of static-to-bootstrap.
		if !a.requireIdentity {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		id, err := strconv.ParseInt(p.Subject, 10, 64)
		if err != nil || id <= 0 {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		token, err := a.store.APITokenByID(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		if err != nil {
			return nil, fmt.Errorf("%w: reading token", mcpevents.ErrAuthorityUnavailable)
		}
		now := a.now()
		if token.RevokedAt != nil || (token.ExpiresAt != nil && !now.Before(*token.ExpiresAt)) ||
			(token.Scope != nil && token.ExpiresAt == nil) {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		resolved := principalFromAPIToken(token)
		principal = &resolved
	case mcpevents.AuthorityStaticToken:
		// The configured token is a static-token principal only outside identity
		// mode. In identity mode it resolves to bootstrap, which cannot subscribe.
		if a.token == "" || a.requireIdentity || a.host != nil || p.Subject != staticTokenSubject(a.token) {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		principal = &Principal{Kind: PrincipalStaticToken}
	case mcpevents.AuthorityLocal:
		if a.token != "" || a.requireIdentity || a.host != nil {
			return nil, mcpevents.ErrPrincipalRevoked
		}
	case mcpevents.AuthorityHost:
		if a.host == nil {
			// Fail closed, but do not end rows: the controller may simply not be
			// wired in this process.
			return nil, fmt.Errorf("%w: no host access controller", mcpevents.ErrAuthorityUnavailable)
		}
		host := Principal{Kind: PrincipalHost, Subject: p.Subject, Actor: p.Actor}
		if !validHostPrincipal(host) {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		policy, ok := hostOperationPolicy("pollEvents")
		if !ok {
			return nil, fmt.Errorf("%w: no host policy", mcpevents.ErrAuthorityUnavailable)
		}
		decision, err := a.host.Authorize(ctx, HostAccessRequest{
			Subject: host.Subject, Actor: host.Actor,
			Operation: HostOperation{ID: "pollEvents", Method: http.MethodGet, Path: "/api/v1/events", AllProjects: true, Policy: policy},
		})
		if errors.Is(err, ErrHostAccessDenied) {
			return nil, mcpevents.ErrPrincipalRevoked
		}
		if err != nil {
			return nil, fmt.Errorf("%w: host access decision", mcpevents.ErrAuthorityUnavailable)
		}
		principal, hostGrant = &host, &decision
	default:
		return nil, mcpevents.ErrPrincipalRevoked
	}
	return a.admit(ctx, principal, hostGrant)
}

// admit builds the project access decision the middleware builds for a
// request, from the same functions.
func (a *mcpEventsAuthority) admit(ctx context.Context, principal *Principal, hostGrant *HostAccessDecision) (mcpevents.Authorized, error) {
	authorized := &mcpEventsAuthorized{store: a.store, principal: principal, host: hostGrant, ownerLocal: principal == nil}
	bound := authorized.attach(ctx)
	if principal != nil && principal.Kind == PrincipalHost {
		bound = context.WithValue(bound, projectHostedContextKey{}, true)
	}
	if principal != nil && principal.Scope != nil {
		if err := revalidateIssueScopedPrincipal(bound, a.store); err != nil {
			return nil, scopedAuthorityError(err)
		}
	}
	owner := projectOwnerAuthority(bound)
	if principal == nil && !owner {
		return nil, mcpevents.ErrPrincipalRevoked
	}
	decision := &ProjectAccessDecision{store: a.store, owner: owner}
	if principal != nil {
		decision.Actor = principal.Actor
	}
	revision, err := a.store.ProjectAccessRevision(bound)
	if err != nil {
		return nil, fmt.Errorf("%w: reading project access revision", mcpevents.ErrAuthorityUnavailable)
	}
	decision.PolicyRevision = revision
	if !owner {
		decision.ProjectUIDs, err = accessibleProjectUIDs(bound, a.store, decision.Actor)
		if err != nil {
			return nil, fmt.Errorf("%w: reading accessible projects", mcpevents.ErrAuthorityUnavailable)
		}
	}
	authorized.decision = decision
	return authorized, nil
}

// scopedAuthorityError maps the 401 a scoped-credential revalidation returns
// to revocation, and anything else to an unavailable decision.
func scopedAuthorityError(err error) error {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusUnauthorized {
		return mcpevents.ErrPrincipalRevoked
	}
	return fmt.Errorf("%w: scoped credential check", mcpevents.ErrAuthorityUnavailable)
}

// mcpEventsAuthorized is one principal's authorization snapshot. It holds
// the pieces of request context the daemon's decision functions read, and
// reattaches them to the caller's context for every operation.
type mcpEventsAuthorized struct {
	store      db.Storage
	principal  *Principal
	host       *HostAccessDecision
	decision   *ProjectAccessDecision
	ownerLocal bool
}

// attach adds the principal and the owner-local fact to ctx.
func (a *mcpEventsAuthorized) attach(ctx context.Context) context.Context {
	if a.principal != nil {
		return WithPrincipal(ctx, *a.principal)
	}
	if a.ownerLocal {
		return context.WithValue(ctx, ownerLocalTransportContextKey{}, true)
	}
	return ctx
}

// bind returns ctx carrying the full decision context, after rechecking the
// parts that can change without notice: project access, host access and
// database credentials.
func (a *mcpEventsAuthorized) bind(ctx context.Context) (context.Context, error) {
	if a.decision == nil {
		return nil, mcpevents.ErrAuthorityUnavailable
	}
	ctx = a.attach(ctx)
	if a.principal != nil && a.principal.Kind == PrincipalHost {
		ctx = context.WithValue(ctx, projectHostedContextKey{}, true)
		if a.host != nil && a.host.Revalidate != nil {
			if err := a.host.Revalidate(ctx); err != nil {
				if errors.Is(err, ErrHostAccessDenied) {
					return nil, mcpevents.ErrPrincipalRevoked
				}
				return nil, fmt.Errorf("%w: host access revalidation", mcpevents.ErrAuthorityUnavailable)
			}
		}
	}
	// Project grants may narrow without revoking the credential. ResolveProjects
	// reevaluates the stored explicit or dynamic selection against this catalog.
	if _, _, _, err := a.decision.refreshProjects(ctx, false); err != nil {
		return nil, unavailableError("project access refresh", err)
	}
	// A private copy: target recording mutates the decision.
	decision := *a.decision
	decision.targets = nil
	ctx = context.WithValue(ctx, projectAccessContextKey{}, &decision)
	ctx = db.WithAuthorizedProjects(ctx, decision.ProjectUIDs)
	if a.principal != nil && a.principal.Kind == PrincipalDBToken {
		if err := revalidateIssueScopedPrincipal(ctx, a.store); err != nil {
			return nil, scopedAuthorityError(err)
		}
	}
	return ctx, nil
}

func unavailableError(what string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusUnauthorized {
		return mcpevents.ErrPrincipalRevoked
	}
	return fmt.Errorf("%w: %s", mcpevents.ErrAuthorityUnavailable, what)
}

// VisibleProjects lists the active projects the principal can see, narrowed
// to selection when it is non-nil. A scoped credential sees only its own
// project, and only while its subtree is non-empty.
func (a *mcpEventsAuthorized) VisibleProjects(ctx context.Context, selection []string) ([]mcpevents.Project, error) {
	ctx, err := a.bind(ctx)
	if err != nil {
		return nil, err
	}
	var projects []db.Project
	if scope := issueScopeFromContext(ctx); scope != nil {
		project, issueIDs, err := issueScopedMembership(ctx, a.store)
		if err != nil {
			return nil, unavailableError("scoped membership", err)
		}
		if len(issueIDs) > 0 && project.DeletedAt == nil && authorizeProjectTarget(ctx, project.UID) == nil {
			projects = []db.Project{project}
		}
	} else if projects, err = a.store.ListProjects(ctx); err != nil {
		return nil, unavailableError("listing projects", err)
	}
	out := make([]mcpevents.Project, 0, len(projects))
	for _, p := range projects {
		if selection != nil && !slices.Contains(selection, p.UID) {
			continue
		}
		out = append(out, mcpevents.Project{UID: p.UID, ID: p.ID, Name: p.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// Scoped reports a credential confined to one issue subtree.
func (a *mcpEventsAuthorized) Scoped() bool {
	return a.principal != nil && a.principal.Scope != nil
}

// AuthorizeOccurrence rechecks that the occurrence's project is active and
// visible and that its issue is still inside the principal's authority,
// including a scoped credential's subtree.
func (a *mcpEventsAuthorized) AuthorizeOccurrence(ctx context.Context, o mcpevents.Occurrence) error {
	ctx, err := a.bind(ctx)
	if err != nil {
		return err
	}
	scope := issueScopeFromContext(ctx)
	if o.ProjectUID != "" {
		project, err := a.store.ProjectByUID(ctx, o.ProjectUID)
		if errors.Is(err, db.ErrNotFound) {
			return mcpevents.ErrOccurrenceHidden
		}
		if err != nil {
			return unavailableError("project", err)
		}
		if project.DeletedAt != nil || authorizeProjectTarget(ctx, project.UID) != nil || (scope != nil && project.UID != scope.ProjectUID) {
			return mcpevents.ErrOccurrenceHidden
		}
	}
	if o.IssueUID == "" {
		return nil
	}
	issue, err := a.store.IssueByUID(ctx, o.IssueUID, db.IncludeDeletedYes)
	if errors.Is(err, db.ErrNotFound) {
		return mcpevents.ErrOccurrenceHidden
	}
	if err != nil {
		return unavailableError("issue", err)
	}
	if scope != nil {
		if err := authorizeIssueScopedIssue(ctx, a.store, issue); err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusNotFound {
				return mcpevents.ErrOccurrenceHidden
			}
			return unavailableError("scoped issue", err)
		}
	}
	return nil
}

// LifecycleMayChangeInbox reads the issue under the same visibility as
// AuthorizeOccurrence and reports whether it holds a valid request for
// recipient. Inbox reads apply the same parser. For a scoped credential every
// visible lifecycle change counts: deleting or restoring a parent removes or
// returns its descendants, and their requests, to the subtree.
func (a *mcpEventsAuthorized) LifecycleMayChangeInbox(ctx context.Context, issueUID, recipient string) (bool, error) {
	ctx, err := a.bind(ctx)
	if err != nil {
		return false, err
	}
	if issueScopeFromContext(ctx) != nil {
		return true, nil
	}
	issue, err := a.store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, unavailableError("issue", err)
	}
	var metadata map[string]any
	if issue.Metadata == "" || json.Unmarshal([]byte(issue.Metadata), &metadata) != nil {
		return false, nil
	}
	_, valid := notification.ParseValue(metadata[notification.MetadataKey(recipient)])
	return valid, nil
}
