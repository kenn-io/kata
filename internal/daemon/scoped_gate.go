package daemon

import "go.kenn.io/kata/internal/api"

// scopedRelayPrivateBuild can only be enabled by the rcpl_private build tag.
// It is not a runtime flag or environment opt-in.
var scopedRelayPrivateBuild bool

// ScopedRelayEnabled reports the private development gate, not completeness of
// the public capability bundle. Instance discovery must not advertise v2 until
// all native stores, transport and ordinary command paths are implemented.
func ScopedRelayEnabled() bool { return scopedRelayPrivateBuild }

func requireScopedRelay() error {
	if !ScopedRelayEnabled() {
		return api.NewError(409, api.ScopedClientUpgradeRequired, "scoped relay support requires a complete compatible build", "", nil)
	}
	return nil
}
