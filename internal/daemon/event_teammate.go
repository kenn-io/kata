package daemon

import (
	"net/http"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/teammate"
)

// withEventTeammate runs after general authentication. Self-authenticating
// claim routes validate contribution when they resolve their principal.
// Contribution never supplies a principal or any comment/routing identity.
func withEventTeammate(next http.Handler, selfAuthenticated selfAuthenticatedRouteMatcher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle := ""
		if _, present := r.Header[http.CanonicalHeaderKey(teammate.Header)]; present {
			handle = r.Header.Get(teammate.Header)
			if !selfAuthenticated.matches(r) {
				if err := teammate.Validate(handle); err != nil {
					api.WriteEnvelope(w, http.StatusBadRequest, "invalid_teammate", err.Error())
					return
				}
			}
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			handle = ""
		}
		// Each request owns its contribution, even if the daemon's parent
		// context inherited a selection when it was launched.
		r = r.WithContext(teammate.WithRequest(r.Context(), handle))
		next.ServeHTTP(w, r)
	})
}
