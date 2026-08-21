package daemon

import (
	"net/http"

	"go.kenn.io/kata/internal/api"
)

// IdleKeepaliveHeader marks a liveness request as foreground activity. It is
// a lifecycle hint, not an authorization mechanism.
const IdleKeepaliveHeader = "X-Kata-Idle-Keepalive"

// IdleForegroundAdmission is the narrow lifecycle seam used by HTTP serving.
type IdleForegroundAdmission interface {
	TryForeground() (*IdleLease, bool)
}

func withIdleAdmission(admission IdleForegroundAdmission, next http.Handler) http.Handler {
	if admission == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browser preflight is validation traffic, not use of the daemon. It may
		// reach this inner layer on public probe routes that intentionally bypass
		// session and bearer requirements.
		if r.Method == http.MethodOptions || isObservationalProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		lease, admitted := admission.TryForeground()
		if !admitted {
			api.WriteEnvelope(w, http.StatusServiceUnavailable, "daemon_stopping",
				"daemon is stopping")
			return
		}
		defer lease.Release()
		next.ServeHTTP(w, r)
	})
}

func isObservationalProbe(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/ping":
		return !isMarkedIdleKeepalive(r)
	case "/api/v1/health", "/api/v1/instance":
		return true
	default:
		return false
	}
}

func isMarkedIdleKeepalive(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/api/v1/ping" &&
		r.Header.Get(IdleKeepaliveHeader) != ""
}
