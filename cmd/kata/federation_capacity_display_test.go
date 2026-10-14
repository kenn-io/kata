package main

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestFederationStatusRelayCapacityPresentation(t *testing.T) {
	const capacity = 64 << 20
	cases := []struct {
		name          string
		bytes         int64
		wantLine      string
		wantWarning   string
		rejectWarning bool
	}{
		{name: "comfortable", bytes: 6 << 20, wantLine: "relay checkpoint: 6.0 MiB of 64.0 MiB", rejectWarning: true},
		{name: "near capacity", bytes: capacity - capacity/20, wantLine: "relay checkpoint: 60.8 MiB of 64.0 MiB", wantWarning: "warning: relay checkpoint is within 10% of capacity; purge issues the project no longer needs before new relays are refused"},
		{name: "over capacity", bytes: capacity + 1, wantLine: "relay checkpoint: 64.0 MiB of 64.0 MiB", wantWarning: "warning: relay checkpoint exceeds capacity; new relay enrollments and resets are refused until issues are purged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := api.FederationProjectStatus{ProjectName: "hub-project", Role: "hub", RelayCheckpointBytes: tc.bytes, RelayCheckpointCapacity: capacity}
			body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{status}}
			var human, agent bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&human)
			require.NoError(t, printFederationStatus(cmd, body))
			require.Contains(t, human.String(), tc.wantLine)
			if tc.rejectWarning {
				require.NotContains(t, human.String(), "warning:")
			} else {
				require.Contains(t, human.String(), tc.wantWarning)
			}
			cmd.SetOut(&agent)
			require.NoError(t, printFederationStatusAgent(cmd, body))
			require.Contains(t, agent.String(), "relay_checkpoint_bytes="+strconv.FormatInt(tc.bytes, 10))
			require.Contains(t, agent.String(), "relay_checkpoint_capacity_bytes="+strconv.Itoa(capacity))
		})
	}
}

func TestFederationStatusRelayCapacityErrorPresentation(t *testing.T) {
	status := api.FederationProjectStatus{ProjectName: "hub-project", Role: "hub", RelayCheckpointError: "relay project checkpoint read failed"}
	body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{status}}
	var human, agent bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&human)
	require.NoError(t, printFederationStatus(cmd, body))
	require.Contains(t, human.String(), "relay checkpoint: not measured; relay project checkpoint read failed")
	require.NotContains(t, human.String(), "MiB")
	cmd.SetOut(&agent)
	require.NoError(t, printFederationStatusAgent(cmd, body))
	require.Contains(t, agent.String(), "relay_checkpoint_error=")
	require.NotContains(t, agent.String(), "relay_checkpoint_bytes=")
}

func TestFederationStatusOmitsRelayCapacityForSpokes(t *testing.T) {
	status := api.FederationProjectStatus{ProjectName: "spoke-project", Role: "spoke"}
	body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{status}}
	var human bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&human)
	require.NoError(t, printFederationStatus(cmd, body))
	require.NotContains(t, human.String(), "relay checkpoint")
}
