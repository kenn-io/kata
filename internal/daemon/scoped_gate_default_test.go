//go:build !rcpl_private

package daemon

import (
	"errors"
	"testing"

	"go.kenn.io/kata/internal/api"
)

func TestScopedGateDefaultRefusal(t *testing.T) {
	if ScopedRelayEnabled() {
		t.Fatal("ordinary build enabled the private scoped gate")
	}
	err := requireScopedRelay()
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("disabled gate returned a non-API error: %v", err)
	}
	if apiErr.GetStatus() != 409 || apiErr.Envelope().Error.Code != api.ScopedClientUpgradeRequired {
		t.Fatalf("disabled gate returned status=%d code=%q, want 409 %q", apiErr.GetStatus(), apiErr.Envelope().Error.Code, api.ScopedClientUpgradeRequired)
	}
}
