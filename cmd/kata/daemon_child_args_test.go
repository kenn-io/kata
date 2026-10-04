package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetachedDaemonReceivesMigrationConsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{{"no consent", false}, {"consent", true}} {
		t.Run(tc.name, func(t *testing.T) {
			args := detachedDaemonArgs("127.0.0.1:0", true, false, tc.want)
			if tc.want {
				require.Contains(t, args, "--allow-dev-migration")
			} else {
				require.NotContains(t, args, "--allow-dev-migration")
			}
			require.Equal(t, []string{"daemon", "start", "--foreground"}, args[:3])
			require.Equal(t, []string{"--listen", "127.0.0.1:0", "--insecure-readonly"}, args[len(args)-3:])
		})
	}
}
