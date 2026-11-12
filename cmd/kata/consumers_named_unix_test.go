package main

import "testing"

func TestUnixConsumerFederationNamedTargets(t *testing.T) {
	for _, selection := range []string{"named", "active"} {
		testUnixConsumerFederationTarget(t, selection)
	}
}
