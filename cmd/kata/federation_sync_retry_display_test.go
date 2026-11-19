package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestFederationStatusSyncRetryPresentation(t *testing.T) {
	next := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		retry api.FederationSyncRetry
		human string
	}{
		{"refused", api.FederationSyncRetry{Reason: "refused", HTTPStatus: 403, Failures: 5, NextAttemptAt: next},
			"sync retry: hub refused the credential (403) on 5 attempts; next attempt 2026-01-01T00:15:00Z"},
		{"unavailable", api.FederationSyncRetry{Reason: "unavailable", HTTPStatus: 503, Failures: 1, NextAttemptAt: next},
			"sync retry: hub unavailable (503) on 1 attempt; next attempt 2026-01-01T00:15:00Z"},
		{"relay links", api.FederationSyncRetry{Reason: "relay_cross_project_links", HTTPStatus: 409, Failures: 2, NextAttemptAt: next},
			"sync retry: relay hub refused cross-project links (409) on 2 attempts; next attempt 2026-01-01T00:15:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{{ProjectName: "spoke-project", Role: "spoke", SyncRetry: &tc.retry}}}
			var human, agent bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&human)
			require.NoError(t, printFederationStatus(cmd, body))
			require.Contains(t, human.String(), tc.human)
			cmd.SetOut(&agent)
			require.NoError(t, printFederationStatusAgent(cmd, body))
			require.Contains(t, agent.String(), "sync_retry="+tc.retry.Reason)
			require.Contains(t, agent.String(), "next_attempt=2026-01-01T00:15:00Z")
		})
	}
}

func TestFederationStatusWithoutSyncRetry(t *testing.T) {
	body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{{ProjectName: "spoke-project", Role: "spoke"}}}
	var human, agent bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&human)
	require.NoError(t, printFederationStatus(cmd, body))
	require.NotContains(t, human.String(), "sync retry")
	cmd.SetOut(&agent)
	require.NoError(t, printFederationStatusAgent(cmd, body))
	require.NotContains(t, agent.String(), "sync_retry")
}
