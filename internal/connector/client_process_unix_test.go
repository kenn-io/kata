//go:build !windows

package connector

import (
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	protocol "go.kenn.io/kata/pkg/connector"
)

// escapeConnectorTree moves a helper descendant into its own process group,
// outside the connector's contained tree.
func escapeConnectorTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// An escaped process is the only one that can keep stdout open after
// everything in the connector's tree has exited. The client must judge the
// complete output once it reaches end of file instead of racing a cleanup
// timer.
func TestProcessClientJudgesOutputHeldOpenOnlyByEscapedProcess(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mode     string
		call     func(*processClient) error
		wantKind error
	}{
		{
			name: "valid response",
			mode: "escaped-stdout-holder-and-exit",
			call: func(client *processClient) error {
				_, err := client.Describe(t.Context())
				return err
			},
		},
		{
			name: "method-specific violation",
			mode: "mismatched-root-escaped-stdout-holder-and-exit",
			call: func(client *processClient) error {
				_, err := client.ReadRoot(t.Context(), protocol.ReadRootParams{RootKey: "requested-root"})
				return err
			},
			wantKind: ErrProtocolFailure,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			readyPath := filepath.Join(t.TempDir(), "ready")
			t.Setenv("HELPER_MODE", tt.mode)
			t.Setenv("HELPER_SYNC", readyPath)
			client := newHelperClient(t, config.ConnectorConfig{
				ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 30,
				Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
			})
			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(client) }()

			pid := waitForProcessClientHelperPID(t, readyPath)
			t.Cleanup(func() { killProcessClientHelper(pid) })
			// The client has no reason to finish while stdout is still open.
			// Ten cleanup grace periods give an implementation that races a
			// timer time to end the call; a correct one is unaffected.
			select {
			case err := <-errCh:
				t.Fatalf("connector call ended while an escaped process held its stdout: %v", err)
			case <-time.After(10 * connectorProcessCleanupGrace):
			}
			killProcessClientHelper(pid)
			err := awaitBoundedProcessClientError(t, errCh, pid)

			if tt.wantKind == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.wantKind)
			assert.NotErrorIs(t, err, ErrProcessFailure)
		})
	}
}

func TestProcessClientTimesOutWhenEscapedProcessNeverClosesStdout(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	t.Setenv("HELPER_MODE", "escaped-stdout-holder-and-exit")
	t.Setenv("HELPER_SYNC", readyPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 1,
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Describe(t.Context())
		errCh <- err
	}()

	pid := waitForProcessClientHelperPID(t, readyPath)
	t.Cleanup(func() { killProcessClientHelper(pid) })
	err := awaitBoundedProcessClientError(t, errCh, pid)

	require.ErrorIs(t, err, ErrRequestTimeout)
	assert.NotErrorIs(t, err, ErrProcessFailure)
	assert.EqualError(t, err, "external connector request timed out")
}

type processClientHelperObservation int

func observeProcessClientHelper(_ *testing.T, pid int) processClientHelperObservation {
	return processClientHelperObservation(pid)
}

func requireProcessClientHelperGone(t *testing.T, observed processClientHelperObservation) {
	t.Helper()
	pid := int(observed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connector descendant %d survived cancellation", pid)
}

func processClientHelperReadyFilePending(error) bool {
	return false
}
