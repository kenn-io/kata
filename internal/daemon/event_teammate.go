package daemon

import (
	"net/http"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

// withEventTeammate runs after authentication.
// Contribution never supplies a principal or any comment/routing identity.
func withEventTeammate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header[http.CanonicalHeaderKey(teammate.Header)]; present {
			handle := r.Header.Get(teammate.Header)
			if err := teammate.Validate(handle); err != nil {
				api.WriteEnvelope(w, http.StatusBadRequest, "invalid_teammate", err.Error())
				return
			}
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				r = r.WithContext(db.WithEventTeammate(r.Context(), handle))
			}
		}
		next.ServeHTTP(w, r)
	})
}
