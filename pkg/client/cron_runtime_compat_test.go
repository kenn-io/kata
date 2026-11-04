package client_test

import (
	"testing"

	"go.kenn.io/kata/pkg/client/generated"
)

// Additive routes must preserve existing generated client enum assignments.
func TestCronRuntimeKeepsExistingEnumConstants(t *testing.T) {
	check := func(unavailable generated.FederationProjectStatusRootSigningKey, stopping generated.IdleShutdownHealthState, expired, live generated.TokenOutState) {
		if unavailable != "unavailable" || stopping != "stopping" || expired != "expired" || live != "live" {
			t.Fatal("existing wire enum changed")
		}
	}
	check(generated.Unavailable, generated.Stopping, generated.Expired, generated.Live)
}
