package daemon

import "github.com/danielgtaylor/huma/v2"

// hostAccessRule is the single source of truth for one route's access facts:
// who is allowed to authenticate it, and how much project authority a decision
// needs before dispatch. Operations with no entry take the conservative
// default (every field false): the daemon bearer token is required, the scope
// comes from {project_id} in the URL, and a route with no URL scope is
// authorized against every project.
type hostAccessRule struct {
	// SelfAuthenticated marks a route whose handler owns authentication: the
	// credential it carries is not the daemon token, so the bearer middleware
	// must not pre-empt it. The middleware's bypass set is generated from this
	// field (see selfAuthenticatedRoutes).
	SelfAuthenticated bool

	// AcceptsFederationBearer additionally lets the host-access middleware
	// dispatch an unattributed bearer request, because the handler will
	// authenticate it as a project-scoped federation enrollment. It is
	// deliberately narrower than SelfAuthenticated: forceReleaseIssueLease
	// bypasses the bearer middleware but passes allowEnrollment=false to
	// resolveClaimPrincipal, so an enrollment credential must not reach it.
	AcceptsFederationBearer bool

	// ResolvedByHandler marks a route whose project scope is discovered during
	// dispatch, so pre-dispatch authorization is deferred to the handler's
	// authorizeHostProjectScope call.
	ResolvedByHandler bool

	// RequiresAllProjects marks a route whose result or side effects can depend
	// on relationships outside the project named in the URL. Requiring complete
	// authority before dispatch keeps denials from becoming project-existence
	// or relationship-state oracles.
	RequiresAllProjects bool

	// NoProjectData marks a route that reads no project data at all, so a
	// missing URL scope must not be widened to every project.
	NoProjectData bool
}

var hostAccessRules = buildHostAccessRules()

func buildHostAccessRules() map[string]hostAccessRule {
	rules := make(map[string]hostAccessRule)
	registerHostAccessRules(rules, hostAccessRule{
		SelfAuthenticated: true, AcceptsFederationBearer: true,
	}, "getFederationProjectMetadata", "pollFederationProjectEvents",
		"ingestFederationProjectEvents", "acquireIssueLease", "renewIssueLease",
		"releaseIssueLease", "getIssueLeaseStatus")
	registerHostAccessRules(rules, hostAccessRule{SelfAuthenticated: true},
		"forceReleaseIssueLease")
	registerHostAccessRules(rules, hostAccessRule{ResolvedByHandler: true},
		"mergeProject", "moveIssue", "listAllIssues", "readUIReferences",
		"readUILaunchTarget", "patchProjectMetadata", "createFederationEnrollment",
		"rotateFederationEnrollment")
	registerHostAccessRules(rules, hostAccessRule{RequiresAllProjects: true},
		"purgeIssue", "purgeProject", "closeIssue", "readyIssues", "showIssueByUID",
		"importIssues", "pollProjectEvents", "streamEvents", "auditCloses",
		"digestProject", "deleteLink", "rewriteAuthorIdentity")
	registerHostAccessRules(rules, hostAccessRule{NoProjectData: true},
		"ping", "health", "instance")
	return rules
}

// registerHostAccessRules mirrors registerHostOperations' discipline: a
// contradictory rule or a duplicate registration is a programming error that
// fails at process start rather than becoming a silent access decision.
func registerHostAccessRules(
	rules map[string]hostAccessRule,
	rule hostAccessRule,
	operationIDs ...string,
) {
	for _, operationID := range operationIDs {
		if conflict := hostAccessRuleConflict(rule); conflict != "" {
			panic("contradictory host access rule for " + operationID + ": " + conflict)
		}
		if _, exists := rules[operationID]; exists {
			panic("duplicate host access rule: " + operationID)
		}
		rules[operationID] = rule
	}
}

// hostAccessRuleConflict reports why a rule asserts two facts that cannot both
// hold, or "" when the rule is internally consistent.
func hostAccessRuleConflict(rule hostAccessRule) string {
	switch {
	case rule.NoProjectData && rule.RequiresAllProjects:
		return "an operation cannot both touch no project data and require " +
			"authority over every project"
	case rule.AcceptsFederationBearer && !rule.SelfAuthenticated:
		return "a federation bearer cannot reach a route the bearer middleware pre-empts"
	default:
		return ""
	}
}

func hostAccessRuleFor(operationID string) hostAccessRule {
	return hostAccessRules[operationID]
}

// routeTemplate is one registered route in http.ServeMux pattern form.
type routeTemplate struct {
	Method string
	Path   string
}

// registeredOperations returns every operation huma registered on doc, keyed by
// operation ID. It is the bridge between the operation-ID key space the access
// rules use and the URL templates the routes are registered with.
func registeredOperations(doc *huma.OpenAPI) map[string]routeTemplate {
	operations := make(map[string]routeTemplate)
	if doc == nil {
		return operations
	}
	for _, pathItem := range doc.Paths {
		if pathItem == nil {
			continue
		}
		for _, operation := range []*huma.Operation{
			pathItem.Get, pathItem.Put, pathItem.Post, pathItem.Delete,
			pathItem.Options, pathItem.Head, pathItem.Patch, pathItem.Trace,
		} {
			if operation == nil {
				continue
			}
			operations[operation.OperationID] = routeTemplate{
				Method: operation.Method, Path: operation.Path,
			}
		}
	}
	return operations
}
