package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// selfAuthenticatedRoutes returns the route templates of every registered
// operation whose access rule says the handler owns authentication. Generating
// the bypass set from the same literal the route is registered with is what
// makes a path rename impossible to get wrong: rename the path in the handler
// file and the matcher follows it.
func selfAuthenticatedRoutes(doc *huma.OpenAPI) []routeTemplate {
	var routes []routeTemplate
	for operationID, template := range registeredOperations(doc) {
		if hostAccessRuleFor(operationID).SelfAuthenticated {
			routes = append(routes, template)
		}
	}
	return routes
}

// selfAuthenticatedRouteMatcher answers "does this request address a route
// whose handler authenticates itself?". It is an http.ServeMux built from the
// registered route templates, so matching is the same method-and-wildcard
// matching the real router performs. The zero value matches nothing, which is
// the safe direction: the daemon bearer token is required.
type selfAuthenticatedRouteMatcher struct {
	mux *http.ServeMux
}

type selfAuthenticatedMatchKey struct{}

func newSelfAuthenticatedRouteMatcher(routes []routeTemplate) selfAuthenticatedRouteMatcher {
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, markSelfAuthenticatedMatch(route.Method))
	}
	return selfAuthenticatedRouteMatcher{mux: mux}
}

// markSelfAuthenticatedMatch records a match for the probe request. The
// {project_id} guard is load-bearing: a handler can only authenticate a request
// against a project it can identify, so a project id that is not a positive
// integer keeps requiring the daemon's bearer token instead of reaching an
// unauthenticated handler.
func markSelfAuthenticatedMatch(method string) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			return
		}
		matched, ok := r.Context().Value(selfAuthenticatedMatchKey{}).(*bool)
		if !ok {
			return
		}
		if raw := r.PathValue("project_id"); raw != "" {
			if _, valid := positiveProjectID(raw); !valid {
				return
			}
		}
		*matched = true
	}
}

func (m selfAuthenticatedRouteMatcher) matches(r *http.Request) bool {
	if m.mux == nil {
		return false
	}
	matched := false
	probe := r.WithContext(context.WithValue(
		r.Context(), selfAuthenticatedMatchKey{}, &matched))
	m.mux.ServeHTTP(discardResponseWriter{}, probe)
	return matched
}

// discardResponseWriter absorbs whatever the probe mux writes for a request
// that matches nothing (its own 404, or a redirect for a non-canonical path);
// only the match flag is read back.
type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header         { return http.Header{} }
func (discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardResponseWriter) WriteHeader(int)             {}
