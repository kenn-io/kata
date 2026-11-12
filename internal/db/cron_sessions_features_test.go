package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronSessionPayloadFeatures(t *testing.T) {
	for _, tc := range []struct{ kind, payload, want string }{
		{"cron.run.observed", `{"summary":{"version":1,"sessions_incomplete":true}}`, "cron_run_sessions_v1,cron_v1"},
		{"cron.run.snapshot", `{"summary":{"version":1,"sessions":[{"agent":"claude","session_id":"00000000-0000-4000-8000-000000000001"}]}}`, "cron_run_sessions_v1,cron_v1"},
		{"cron.run.observed", `{"summary":{"version":1}}`, "cron_v1"},
		{"issue.updated", `{"summary":{"sessions":[]}}`, ""},
	} {
		got, err := db.PayloadRequiredFeatures(tc.kind, tc.payload)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
}

// Pushes from any writer, including restored data, must declare the sessions feature.
func TestCronSessionPayloadWireVersion(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload, want string
		version                   int
	}{
		{"sessions", "cron.run.observed", `{"summary":{"version":1,"sessions_incomplete":true}}`, "cron_run_sessions_v1,cron_v1", 31},
		{"no sessions", "cron.run.observed", `{"summary":{"version":1}}`, "cron_v1", 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version, feature, err := db.FederationEventPayloadWireVersion(tc.kind, []byte(tc.payload))
			require.NoError(t, err)
			require.Equal(t, tc.version, version)
			require.Equal(t, tc.want, feature)
		})
	}
}
