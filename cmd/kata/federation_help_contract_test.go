package main

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/federation"
)

func TestFederationHelpAdvertisesSupportedCapabilities(t *testing.T) {
	for _, command := range []string{"enroll", "join"} {
		t.Run(command, func(t *testing.T) {
			cmd := federationEnrollCmd()
			if command == "join" {
				cmd = federationJoinCmd()
			}
			var out bytes.Buffer
			cmd.SetOut(&out)
			require.NoError(t, cmd.Help())
			match := regexp.MustCompile("comma-separated capabilities: ([^ ]+)").FindStringSubmatch(out.String())
			require.Len(t, match, 2)
			_, err := federation.NormalizeCapabilities(match[1])
			require.NoError(t, err, "advertised capability bundle must be accepted")
		})
	}
}
