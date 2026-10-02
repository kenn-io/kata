package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/storeopen"
)

func TestDetachedDaemonReceivesMigrationConsent(t *testing.T) {
	for _, tc := range []struct {
		name, env  string
		flag, want bool
	}{
		{"no consent", "", false, false}, {"flag consent", "", true, true},
		{"environment consent", "1", false, true}, {"invalid environment", "true", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KATA_ALLOW_DEV_MIGRATION", tc.env)
			ctx := storeopen.WithDevMigrationAllowed(t.Context(), tc.flag)
			args := detachedDaemonArgs(ctx, "127.0.0.1:0", true)
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
