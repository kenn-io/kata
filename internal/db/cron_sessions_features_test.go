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
