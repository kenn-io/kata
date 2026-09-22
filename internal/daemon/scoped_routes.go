package daemon

import (
	"net/http"
	"regexp"
)

type scopedRouteRule struct {
	method  string
	pattern *regexp.Regexp
}

func scopedRoute(method, pattern string) scopedRouteRule {
	return scopedRouteRule{method: method, pattern: regexp.MustCompile("^" + pattern + "$")}
}

// issueScopedRoutes is the fixed ordinary-work allowlist for native
// issue-subtree credentials. Target membership and all-effects checks happen
// at the domain boundary; this first gate ensures new or administrative routes
// fail closed before their handlers resolve any target.
var issueScopedRoutes = []scopedRouteRule{
	scopedRoute(http.MethodGet, `/api/v1/instance`),
	scopedRoute(http.MethodGet, `/api/v1/health`),
	scopedRoute(http.MethodGet, `/api/v1/projects`),
	scopedRoute(http.MethodPost, `/api/v1/projects/resolve`),
	scopedRoute(http.MethodGet, `/api/v1/projects/[^/]+`),
	scopedRoute(http.MethodGet, `/api/v1/issues(?:/[^/]+)?`),
	scopedRoute(http.MethodGet, `/api/v1/(?:ready|digest|events|events/stream|audit/closes)`),
	scopedRoute(http.MethodGet, `/api/v1/projects/[^/]+/(?:digest|events|issues|labels|ready|search)`),
	scopedRoute(http.MethodPost, `/api/v1/projects/[^/]+/issues`),
	scopedRoute(http.MethodGet, `/api/v1/projects/[^/]+/issues/[^/]+`),
	scopedRoute(http.MethodGet, `/api/v1/projects/[^/]+/issues/[^/]+/metadata`),
	scopedRoute(http.MethodPatch, `/api/v1/projects/[^/]+/issues/[^/]+`),
	scopedRoute(http.MethodPost, `/api/v1/projects/[^/]+/issues/[^/]+/actions/(?:assign|claim|close|priority|reopen|unassign)`),
	scopedRoute(http.MethodPost, `/api/v1/projects/[^/]+/issues/[^/]+/(?:comments|labels|links|metadata)`),
	scopedRoute(http.MethodPatch, `/api/v1/projects/[^/]+/issues/[^/]+/comments/[^/]+`),
	scopedRoute(http.MethodDelete, `/api/v1/projects/[^/]+/issues/[^/]+/(?:labels|links)/[^/]+`),
	scopedRoute(http.MethodGet, `/api/v1/projects/[^/]+/issues/[^/]+/(?:graph|lease)`),
	scopedRoute(http.MethodPost, `/api/v1/projects/[^/]+/issues/[^/]+/lease/actions/(?:acquire|release|renew)`),
	scopedRoute(http.MethodGet, `/api/v1/ui/(?:issue-reference|references|snapshot)`),
}

func issueScopedRouteAllowed(method, path string) bool {
	for _, rule := range issueScopedRoutes {
		if method == rule.method && rule.pattern.MatchString(path) {
			return true
		}
	}
	return false
}
