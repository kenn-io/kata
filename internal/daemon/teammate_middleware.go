package daemon

import (
	"net/http"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

// withRequestTeammate accepts native-client attribution after authentication.
func withRequestTeammate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("X-Kata-Teammate")
		if len(values) > 1 {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", "X-Kata-Teammate must have exactly one value")
			return
		}
		handle := r.Header.Get("X-Kata-Teammate")
		if err := teammate.Validate(handle); err != nil {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		if handle != "" && !webSessionAuthenticated(r.Context()) {
			r = r.WithContext(db.WithEventTeammate(r.Context(), handle))
		}
		next.ServeHTTP(w, r)
	})
}

// effectiveRecordTeammate preserves explicit suppression instead of inheriting
// request attribution. Record attribution is independent of reply addressing.
func effectiveRecordTeammate(body *string, request string) string {
	if body != nil {
		return *body
	}
	return request
}
