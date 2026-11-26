package client

import "testing"

func TestUnixConsumerWebUINamedTargets(t *testing.T) {
	for _, selection := range []string{"named", "active"} {
		testUnixConsumerWebUIRejectsSelectedSocket(t, selection)
	}
}
