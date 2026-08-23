package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	protocol "go.kenn.io/kata/pkg/connector"
)

func TestProcessClientPassesOneRequestAndOnlyAllowedEnvironment(t *testing.T) {
	t.Setenv("CONNECTOR_SECRET", "sensitive")
	t.Setenv("UNRELATED_SECRET", "must-not-cross")
	got, err := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t),
		Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env:  map[string]string{"TOKEN": "CONNECTOR_SECRET"},
	}).Describe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "fake.connector", got.ConnectorID)
}

func TestProcessClientSendsEmptySettingsObjectWhenUnconfigured(t *testing.T) {
	t.Setenv("HELPER_MODE", "require-empty-settings")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"MODE": "HELPER_MODE"},
	})

	_, err := client.Describe(t.Context())
	require.NoError(t, err)
}

func TestMinimalRuntimeEnvPreservesWindowsNativePaths(t *testing.T) {
	t.Setenv("TEMP", `C:\runtime\temp`)
	t.Setenv("TMP", `C:\runtime\tmp`)
	t.Setenv("USERPROFILE", `C:\Users\connector`)

	assert.Subset(t, minimalRuntimeEnv("windows"), []string{
		`TEMP=C:\runtime\temp`,
		`TMP=C:\runtime\tmp`,
		`USERPROFILE=C:\Users\connector`,
	})
}

func TestProcessClientRejectsUnsafeResponses(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mode     string
		wantKind error
		want     string
	}{
		{name: "wrong ID", mode: "wrong-id", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "unsupported version", mode: "unsupported-version", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "result and error", mode: "both", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "no terminal value", mode: "neither", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "null result", mode: "null-result", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "malformed JSON", mode: "malformed", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "invalid UTF-8", mode: "invalid-utf8", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "trailing JSON", mode: "trailing", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "oversized response", mode: "oversized", wantKind: ErrProtocolFailure, want: "external connector protocol failed"},
		{name: "nonzero exit hides stderr", mode: "exit", wantKind: ErrProcessFailure, want: "external connector process failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HELPER_MODE", tt.mode)
			client := newHelperClient(t, config.ConnectorConfig{
				ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
				Env: map[string]string{"MODE": "HELPER_MODE"},
			})
			_, err := client.Describe(t.Context())
			require.ErrorIs(t, err, tt.wantKind)
			assert.EqualError(t, err, tt.want)
			assert.NotContains(t, err.Error(), "sensitive stderr")
		})
	}
}

func TestProcessClientRejectsMethodSpecificResponseViolations(t *testing.T) {
	validOtherRoot := `{"key":"root-other","identity_key":"account-1","state":"open","revision":"revision-1","updated_at":"2026-08-20T10:00:00Z","observed_at":"2026-08-20T10:01:00Z"}`
	validOtherCompletedRoot := `{"key":"root-other","identity_key":"account-1","state":"complete","revision":"revision-2","updated_at":"2026-08-20T10:00:00Z","observed_at":"2026-08-20T10:01:00Z"}`
	validOpenRoot := `{"key":"root-1","identity_key":"account-1","state":"open","revision":"revision-1","updated_at":"2026-08-20T10:00:00Z","observed_at":"2026-08-20T10:01:00Z"}`
	for _, tt := range []struct {
		name   string
		result string
		call   func(Client) error
	}{
		{name: "describe missing identities", result: `{}`, call: func(client Client) error {
			_, err := client.Describe(t.Context())
			return err
		}},
		{name: "resolve missing identities and timestamps", result: `{}`, call: func(client Client) error {
			_, err := client.ResolveRoot(t.Context(), protocol.ResolveRootParams{Locator: "root-locator"})
			return err
		}},
		{name: "read root key mismatch", result: validOtherRoot, call: func(client Client) error {
			_, err := client.ReadRoot(t.Context(), protocol.ReadRootParams{RootKey: "root-1"})
			return err
		}},
		{name: "completion state mismatch", result: validOpenRoot, call: func(client Client) error {
			_, err := client.CompleteRoot(t.Context(), protocol.CompleteRootParams{RootKey: "root-1"})
			return err
		}},
		{name: "completion root key mismatch", result: validOtherCompletedRoot, call: func(client Client) error {
			_, err := client.CompleteRoot(t.Context(), protocol.CompleteRootParams{RootKey: "root-1"})
			return err
		}},
		{name: "comment missing identity and timestamps", result: `{"comments":[{}]}`, call: func(client Client) error {
			_, err := client.ListComments(t.Context(), protocol.ListCommentsParams{RootKey: "root-1"})
			return err
		}},
		{name: "comment missing revision", result: `{"comments":[{"id":"comment-1","body":"Body","author":{"id":"actor-1","display_name":"Actor"},"created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:00:00Z"}]}`, call: func(client Client) error {
			_, err := client.ListComments(t.Context(), protocol.ListCommentsParams{RootKey: "root-1"})
			return err
		}},
		{name: "comment padded revision", result: `{"comments":[{"id":"comment-1","revision":" comment-revision-1 ","body":"Body","author":{"id":"actor-1","display_name":"Actor"},"created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:00:00Z"}]}`, call: func(client Client) error {
			_, err := client.ListComments(t.Context(), protocol.ListCommentsParams{RootKey: "root-1"})
			return err
		}},
		{name: "publication missing identity and timestamps", result: `{}`, call: func(client Client) error {
			_, err := client.PublishComment(t.Context(), protocol.PublishCommentParams{RootKey: "root-1", Body: "Body", OperationID: "operation-1"})
			return err
		}},
		{name: "field descriptor missing identity", result: `{"fields":[{}]}`, call: func(client Client) error {
			_, err := client.ListFields(t.Context())
			return err
		}},
		{name: "read field selector mismatch", result: `{"fields":{"field-other":{"kind":"date","value":"2026-08-20"}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "read field missing kind", result: `{"fields":{"field-1":{}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "read field invalid date", result: `{"fields":{"field-1":{"kind":"date","value":"tomorrow"}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "read field noncanonical instant", result: `{"fields":{"field-1":{"kind":"instant","value":"2026-08-20T10:00:00+00:00"}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "read field invalid local datetime", result: `{"fields":{"field-1":{"kind":"local_datetime","value":"2026-08-20 10:00:00","timezone":"Europe/Paris"}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "read field invalid timezone", result: `{"fields":{"field-1":{"kind":"local_datetime","value":"2026-08-20T10:00:00","timezone":"Mars/Olympus"}}}`, call: func(client Client) error {
			_, err := client.ReadFields(t.Context(), protocol.ReadFieldsParams{RootKey: "root-1", FieldIDs: []string{"field-1"}})
			return err
		}},
		{name: "write field readback mismatch", result: `{"fields":{"field-1":{"kind":"date","value":"2026-08-21"}}}`, call: func(client Client) error {
			_, err := client.WriteFields(t.Context(), protocol.WriteFieldsParams{RootKey: "root-1", Fields: map[string]protocol.FieldValue{
				"field-1": {Kind: "date", Value: "2026-08-20"},
			}})
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HELPER_MODE", "raw-result")
			t.Setenv("HELPER_RESULT", tt.result)
			client := newHelperClient(t, config.ConnectorConfig{
				ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
				Env: map[string]string{"MODE": "HELPER_MODE", "RESULT": "HELPER_RESULT"},
			})

			err := tt.call(client)
			require.ErrorIs(t, err, ErrProtocolFailure)
			assert.EqualError(t, err, "external connector protocol failed")
		})
	}
}

func TestProcessClientRejectsMismatchedPublishedComment(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result string
	}{
		{name: "body", result: `{"id":"comment-1","revision":"comment-revision-1","body":"Different body","author":{"id":"actor-self","display_name":"Connector Actor"},"created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:00:00Z"}`},
		{name: "deleted", result: `{"id":"comment-1","revision":"comment-revision-1","body":"Body","author":{"id":"actor-self","display_name":"Connector Actor"},"created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:00:00Z","deleted":true}`},
		{name: "author", result: `{"id":"comment-1","revision":"comment-revision-1","body":"Body","author":{"id":"actor-other","display_name":"Other Actor"},"created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:00:00Z"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HELPER_MODE", "publication-result")
			t.Setenv("HELPER_RESULT", tt.result)
			client := newHelperClient(t, config.ConnectorConfig{
				ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
				Env: map[string]string{"MODE": "HELPER_MODE", "RESULT": "HELPER_RESULT"},
			})

			_, err := client.PublishComment(t.Context(), protocol.PublishCommentParams{
				RootKey: "root-1", Body: "Body", OperationID: "operation-1",
			})
			require.ErrorIs(t, err, ErrProtocolFailure)
			assert.EqualError(t, err, "external connector protocol failed")
		})
	}
}

func TestProcessClientRejectsInvalidPublicationOperationIDBeforeLaunch(t *testing.T) {
	for _, operationID := range []string{"", " operation-1 "} {
		t.Run(strconv.Quote(operationID), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "launched")
			t.Setenv("HELPER_MODE", "launch-marker")
			t.Setenv("HELPER_SYNC", marker)
			client := newHelperClient(t, config.ConnectorConfig{
				ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
				Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
			})

			_, err := client.PublishComment(t.Context(), protocol.PublishCommentParams{
				RootKey: "root-1", Body: "Body", OperationID: operationID,
			})
			require.ErrorContains(t, err, "operation_id")
			_, statErr := os.Stat(marker)
			require.ErrorIs(t, statErr, fs.ErrNotExist)
		})
	}
}

func TestProcessClientRejectsInvalidWriteFieldBeforeLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	t.Setenv("HELPER_MODE", "launch-marker")
	t.Setenv("HELPER_SYNC", marker)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})

	_, err := client.WriteFields(t.Context(), protocol.WriteFieldsParams{
		RootKey: "root-1",
		Fields:  map[string]protocol.FieldValue{"field-1": {Kind: "date", Value: "tomorrow"}},
	})
	require.ErrorContains(t, err, "field value")
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestProcessClientExecutableFailureDoesNotExposePath(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "private-executable-name")
	client := newHelperClient(t, config.ConnectorConfig{ID: "notes", Command: privatePath})

	_, err := client.Describe(t.Context())

	require.ErrorIs(t, err, ErrProcessFailure)
	assert.EqualError(t, err, "external connector process failed")
	assert.NotContains(t, err.Error(), privatePath)
	assert.NotContains(t, err.Error(), "private-executable-name")
}

func TestProcessClientFailureCauseIsInspectableButNotRendered(t *testing.T) {
	t.Setenv("HELPER_MODE", "exit")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"MODE": "HELPER_MODE"},
	})

	_, err := client.Describe(t.Context())

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.EqualError(t, err, "external connector process failed")
	assert.NotContains(t, err.Error(), "sensitive stderr")
}

func TestAwaitCommandOutcomePreservesRecordedProcessResultAfterParentEnds(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	waitResult := make(chan error, 1)
	processErr := errors.New("opaque process failure detail")
	waitResult <- processErr
	cancel()
	killCalls := 0

	runErr, contextCause := awaitCommandOutcome(parent, waitResult, func() error {
		killCalls++
		return nil
	})

	assert.Same(t, processErr, runErr)
	assert.NoError(t, contextCause)
	assert.Zero(t, killCalls)
}

func TestAwaitCommandOutcomeContextWinnerKillsAndWaitsOnce(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	waitResult := make(chan error, 1)
	killed := make(chan struct{}, 1)
	killCalls := 0
	type result struct {
		runErr       error
		contextCause error
	}
	finished := make(chan result, 1)
	go func() {
		runErr, contextCause := awaitCommandOutcome(parent, waitResult, func() error {
			killCalls++
			killed <- struct{}{}
			return nil
		})
		finished <- result{runErr: runErr, contextCause: contextCause}
	}()

	cancel()
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("context winner did not kill the child")
	}
	select {
	case <-finished:
		t.Fatal("context winner returned before the child was reaped")
	default:
	}
	waitResult <- errors.New("synthetic killed process result")
	got := <-finished
	assert.NoError(t, got.runErr)
	assert.ErrorIs(t, got.contextCause, context.Canceled)
	assert.Equal(t, 1, killCalls)
}

func TestAwaitCommandOutcomeTerminationFailureIsBounded(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	waitResult := make(chan error, 1)
	terminationErr := errors.New("opaque termination failure")
	finished := make(chan struct {
		runErr       error
		contextCause error
	}, 1)
	go func() {
		runErr, contextCause := awaitCommandOutcome(parent, waitResult, func() error {
			return terminationErr
		})
		finished <- struct {
			runErr       error
			contextCause error
		}{runErr: runErr, contextCause: contextCause}
	}()
	cancel()

	select {
	case result := <-finished:
		assert.ErrorIs(t, result.runErr, terminationErr)
		assert.NoError(t, result.contextCause)
	case <-time.After(3 * connectorProcessCleanupGrace):
		waitResult <- errors.New("cleanup after failed bounded assertion")
		t.Fatal("termination failure left connector cleanup waiting indefinitely")
	}
}

func TestAwaitCommandOutcomeReapWaitIsBounded(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	waitResult := make(chan error, 1)
	finished := make(chan struct {
		runErr       error
		contextCause error
	}, 1)
	go func() {
		runErr, contextCause := awaitCommandOutcome(parent, waitResult, func() error { return nil })
		finished <- struct {
			runErr       error
			contextCause error
		}{runErr: runErr, contextCause: contextCause}
	}()
	cancel()

	select {
	case result := <-finished:
		require.Error(t, result.runErr)
		assert.NoError(t, result.contextCause)
	case <-time.After(3 * connectorProcessCleanupGrace):
		waitResult <- errors.New("cleanup after failed bounded assertion")
		t.Fatal("connector cleanup waited indefinitely for process reap")
	}
}

func TestConnectorContextErrorKeepsChildTimeoutAfterParentEnds(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	commandCtx, cancelCommand := context.WithTimeoutCause(
		parent, time.Nanosecond, errConnectorChildTimeout,
	)
	defer cancelCommand()
	<-commandCtx.Done()
	require.ErrorIs(t, context.Cause(commandCtx), errConnectorChildTimeout)
	cancelParent()

	err := connectorContextError(parent, context.Cause(commandCtx))

	require.ErrorIs(t, err, ErrRequestTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.EqualError(t, err, "external connector request timed out")
}

func TestProcessClientAlreadyEndedParentWinsBeforeStart(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "private-executable-name")
	client := newHelperClient(t, config.ConnectorConfig{ID: "notes", Command: privatePath})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := client.Describe(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrProcessFailure)
	assert.NotErrorIs(t, err, ErrProtocolFailure)
	assert.NotErrorIs(t, err, ErrRequestTimeout)
	assert.NotContains(t, err.Error(), privatePath)
}

func TestProcessClientBoundsTimeoutAndDoesNotExposeSettings(t *testing.T) {
	t.Setenv("HELPER_MODE", "sleep")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 1,
		Env: map[string]string{"MODE": "HELPER_MODE"}, Settings: map[string]any{"private": "not-for-errors"},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Describe(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrRequestTimeout)
	assert.NotErrorIs(t, err, ErrProcessFailure)
	assert.NotErrorIs(t, err, ErrProtocolFailure)
	assert.NotContains(t, err.Error(), "not-for-errors")
}

func TestProcessClientAppliesConfiguredTimeoutWithoutCallerDeadline(t *testing.T) {
	t.Setenv("HELPER_MODE", "sleep")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 1,
		Env: map[string]string{"MODE": "HELPER_MODE"},
	})

	started := time.Now()
	_, err := client.Describe(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRequestTimeout)
	assert.EqualError(t, err, "external connector request timed out")
	assert.Less(t, time.Since(started), 1500*time.Millisecond)
}

func TestProcessClientConfiguredTimeoutBoundsDescendantHeldStdout(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	t.Setenv("HELPER_MODE", "spawn-stdout-holder")
	t.Setenv("HELPER_SYNC", readyPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 1,
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Describe(context.Background())
		errCh <- err
	}()

	pid := waitForProcessClientHelperPID(t, readyPath)
	descendant := observeProcessClientHelper(t, pid)
	t.Cleanup(func() { killProcessClientHelper(pid) })
	started := time.Now()
	err := awaitBoundedProcessClientError(t, errCh, pid)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRequestTimeout)
	assert.NotErrorIs(t, err, ErrProcessFailure)
	assert.NotErrorIs(t, err, ErrProtocolFailure)
	assert.EqualError(t, err, "external connector request timed out")
	assert.Less(t, time.Since(started), 3*time.Second)
	requireProcessClientHelperGone(t, descendant)
}

func TestProcessClientParentCancellationBoundsDescendantHeldStdout(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	t.Setenv("HELPER_MODE", "spawn-stdout-holder")
	t.Setenv("HELPER_SYNC", readyPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 30,
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Describe(ctx)
		errCh <- err
	}()

	pid := waitForProcessClientHelperPID(t, readyPath)
	descendant := observeProcessClientHelper(t, pid)
	t.Cleanup(func() { killProcessClientHelper(pid) })
	started := time.Now()
	cancel()
	err := awaitBoundedProcessClientError(t, errCh, pid)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrRequestTimeout)
	assert.NotErrorIs(t, err, ErrProcessFailure)
	assert.NotErrorIs(t, err, ErrProtocolFailure)
	assert.EqualError(t, err, context.Canceled.Error())
	assert.Less(t, time.Since(started), 3*time.Second)
	requireProcessClientHelperGone(t, descendant)
}

func TestProcessClientSuccessfulCallCleansBackgroundDescendant(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	t.Setenv("HELPER_MODE", "spawn-background-holder")
	t.Setenv("HELPER_SYNC", readyPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 5,
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})

	_, err := client.Describe(t.Context())
	require.NoError(t, err)
	pid := waitForProcessClientHelperPID(t, readyPath)
	descendant := observeProcessClientHelper(t, pid)
	t.Cleanup(func() { killProcessClientHelper(pid) })
	requireProcessClientHelperGone(t, descendant)
}

func TestProcessClientWaitDelayCleansDescendantHoldingStdout(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	t.Setenv("HELPER_MODE", "spawn-stdout-holder-and-exit")
	t.Setenv("HELPER_SYNC", readyPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"}, TimeoutSeconds: 5,
		Env: map[string]string{"MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})

	_, err := client.Describe(t.Context())
	require.ErrorIs(t, err, ErrProcessFailure)
	pid := waitForProcessClientHelperPID(t, readyPath)
	descendant := observeProcessClientHelper(t, pid)
	t.Cleanup(func() { killProcessClientHelper(pid) })
	requireProcessClientHelperGone(t, descendant)
}

func TestProcessClientRejectsMissingEnvironmentSourceWithoutValue(t *testing.T) {
	const privateSource = "MISSING_CONNECTOR_SECRET"
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": privateSource},
	})
	_, err := client.Describe(t.Context())
	require.ErrorIs(t, err, ErrProcessFailure)
	assert.EqualError(t, err, "external connector process failed")
	assert.NotContains(t, err.Error(), privateSource)
}

func TestProcessClientRedactsMappedEnvironmentValuesFromStructuredErrors(t *testing.T) {
	t.Setenv("CONNECTOR_SECRET", "sensitive")
	t.Setenv("HELPER_MODE", "structured-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sensitive")
}

func TestProcessClientPreservesNonSecretConfiguredArgumentsInStructuredErrors(t *testing.T) {
	const argumentValue = "argument-redaction-value"
	t.Setenv("HELPER_MODE", "argument-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t),
		Args: []string{
			"-test.run=^TestProcessClientHelper$", "--", "--label=" + argumentValue,
		},
		Env: map[string]string{"MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--label="+argumentValue)
	assert.Contains(t, err.Error(), argumentValue)
}

func TestProcessClientRedactsEscapedMappedEnvironmentValuesFromStructuredErrors(t *testing.T) {
	const secret = "line\n\"quote\"\\<>&" // #nosec G101 -- synthetic redaction fixture, not a credential.
	const encoded = `"line\n\"quote\"\\\u003c\u003e\u0026"`
	t.Setenv("CONNECTOR_SECRET", secret)
	t.Setenv("HELPER_MODE", "encoded-structured-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), encoded)
}

func TestProcessClientRedactsSolidusEscapedMappedEnvironmentValuesFromStructuredErrors(t *testing.T) {
	const secret = "a/b"
	const solidusEscaped = `"a\/b"`
	t.Setenv("CONNECTOR_SECRET", secret)
	t.Setenv("HELPER_MODE", "solidus-escaped-environment-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), solidusEscaped)
}

func TestProcessClientRedactsHTMLUnescapedJSONTokenFromStructuredErrors(t *testing.T) {
	const secret = "line\n\"quote\"\\<>&"           // #nosec G101 -- synthetic redaction fixture, not a credential.
	const unescapedToken = `"line\n\"quote\"\\<>&"` // #nosec G101 -- encoded form of the synthetic fixture.
	t.Setenv("CONNECTOR_SECRET", secret)
	t.Setenv("HELPER_MODE", "html-unescaped-json-token")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), unescapedToken)
}

func TestProcessClientRedactsEmbeddedJSONEncodedEnvironmentValuesFromStructuredErrors(t *testing.T) {
	const secret = "line\n\"quote\"\\<>&/tail" // #nosec G101 -- synthetic redaction fixture, not a credential.
	const escapedBody = `line\n\"quote\"\\\u003c\u003e\u0026\/tail`
	const htmlUnescapedBody = `line\n\"quote\"\\<>&\/tail`
	const unicodeSolidusBody = `line\n\"quote\"\\\u003c\u003e\u0026\u002ftail`
	const unicodeSolidusUpperBody = `line\n\"quote\"\\\u003c\u003e\u0026\u002Ftail`
	const mixedHTMLBody = `line\n\"quote\"\\\u003C\u003e\u0026\/tail`
	const partiallyEscapedHTMLBody = `line\n\"quote\"\\\u003c>&\/tail`
	t.Setenv("CONNECTOR_SECRET", secret)
	t.Setenv("HELPER_MODE", "embedded-encoded-environment-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), escapedBody)
	assert.NotContains(t, err.Error(), htmlUnescapedBody)
	assert.NotContains(t, err.Error(), unicodeSolidusBody)
	assert.NotContains(t, err.Error(), unicodeSolidusUpperBody)
	assert.NotContains(t, err.Error(), mixedHTMLBody)
	assert.NotContains(t, err.Error(), partiallyEscapedHTMLBody)
	assert.Equal(t, 6, strings.Count(err.Error(), "[redacted]"))
}

func TestProcessClientRedactsUnicodeSolidusJSONTokensFromStructuredErrors(t *testing.T) {
	const secret = "a/b"
	const lowerToken = `"a\u002fb"` // #nosec G101 -- encoded form of the synthetic fixture.
	const upperToken = `"a\u002Fb"` // #nosec G101 -- encoded form of the synthetic fixture.
	t.Setenv("CONNECTOR_SECRET", secret)
	t.Setenv("HELPER_MODE", "unicode-solidus-json-tokens")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE"},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), lowerToken)
	assert.NotContains(t, err.Error(), upperToken)
}

func TestRedactErrorRedactsOverlappingValuesLongestFirst(t *testing.T) {
	err := redactError(&protocol.Error{Code: "bad", Message: "prefix-suffix"}, []string{"prefix", "prefix-suffix"})
	assert.NotContains(t, err.Error(), "prefix-suffix")
	assert.NotContains(t, err.Error(), "suffix")
}

func TestRedactErrorPreservesUnrelatedEscapeSequences(t *testing.T) {
	err := redactError(&protocol.Error{Code: "bad", Message: `path C:\temp\notes`}, []string{"other-secret"})
	assert.EqualError(t, err, `bad: path C:\temp\notes`)
}

func TestRedactErrorRedactsEmbeddedJSONEncodedAstralSecret(t *testing.T) {
	err := redactError(
		&protocol.Error{Code: "bad", Message: `prefix token-\uD83D\uDE00 suffix`},
		[]string{"token-😀"},
	)
	assert.EqualError(t, err, "bad: prefix [redacted] suffix")
}

func TestProcessClientRedactsEnvironmentSnapshotAfterParentRotation(t *testing.T) {
	syncPath := filepath.Join(t.TempDir(), "connector-ready")
	t.Setenv("CONNECTOR_SECRET", "initial-secret")
	t.Setenv("HELPER_MODE", "delayed-structured-secret")
	t.Setenv("HELPER_SYNC", syncPath)
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"TOKEN": "CONNECTOR_SECRET", "MODE": "HELPER_MODE", "SYNC": "HELPER_SYNC"},
	})

	errs := make(chan error, 1)
	go func() {
		_, err := client.Describe(t.Context())
		errs <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(syncPath)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	t.Setenv("CONNECTOR_SECRET", "rotated-secret")
	err := <-errs
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "initial-secret")
}

func TestProcessClientPreservesNonSecretSettingsScalarsInStructuredErrors(t *testing.T) {
	t.Setenv("HELPER_MODE", "settings-secret")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"MODE": "HELPER_MODE"}, Settings: map[string]any{"enabled": true, "retries": 7},
	})
	_, err := client.Describe(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"enabled":true`)
	assert.Contains(t, err.Error(), `"retries":7`)
}

func newHelperClient(t *testing.T, cfg config.ConnectorConfig) *processClient {
	t.Helper()
	normalized, err := config.NormalizeConnectorConfigs([]config.ConnectorConfig{cfg})
	require.NoError(t, err)
	return newProcessClient(normalized[0])
}

func helperBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	require.NoError(t, err)
	return path
}

func TestProcessClientStdoutHolderHelper(_ *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--connector-stdout-holder") {
		return
	}
	time.Sleep(24 * time.Hour)
}

func TestProcessClientHelper(_ *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestProcessClientHelper") {
		return
	}
	serveProcessClientHelper()
	os.Exit(0)
}

func serveProcessClientHelper() {
	if os.Getenv("MODE") == "launch-marker" {
		if err := os.WriteFile(os.Getenv("SYNC"), []byte("launched"), 0o600); err != nil { // #nosec G703 -- test-owned temporary path.
			os.Exit(11)
		}
	}
	var request protocol.Request
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(2)
	}
	if os.Getenv("MODE") == "sleep" {
		time.Sleep(time.Second)
		return
	}
	if os.Getenv("MODE") == "spawn-stdout-holder" ||
		os.Getenv("MODE") == "spawn-stdout-holder-and-exit" ||
		os.Getenv("MODE") == "spawn-background-holder" {
		child := exec.Command( // #nosec G204,G702 -- the test starts its own fixed executable and helper arguments.
			os.Args[0], "-test.run=^TestProcessClientStdoutHolderHelper$", "--", "--connector-stdout-holder",
		)
		if os.Getenv("MODE") == "spawn-stdout-holder" || os.Getenv("MODE") == "spawn-stdout-holder-and-exit" {
			child.Stdout = os.Stdout
		}
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		readyPath := os.Getenv("SYNC")
		readyTempPath := readyPath + ".tmp"
		if err := os.WriteFile(readyTempPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil { // #nosec G703 -- test-owned temporary path.
			os.Exit(9)
		}
		if err := os.Rename(readyTempPath, readyPath); err != nil { // #nosec G703 -- test-owned temporary paths.
			os.Exit(10)
		}
		if os.Getenv("MODE") == "spawn-stdout-holder" {
			time.Sleep(24 * time.Hour)
			return
		}
	}
	if os.Getenv("MODE") == "exit" {
		_, _ = fmt.Fprintln(os.Stderr, "sensitive stderr")
		os.Exit(7)
	}
	response := protocol.Response{Protocol: protocol.ProtocolVersion, ID: request.ID}
	observed := observedEnvironment()
	if !reflect.DeepEqual(observed, expectedEnvironment(os.Getenv("MODE"))) {
		response.Error = &protocol.Error{Code: "environment", Message: "unexpected connector environment"}
		_ = json.NewEncoder(os.Stdout).Encode(response)
		return
	}
	switch os.Getenv("MODE") {
	case "wrong-id":
		response.ID = "other"
	case "unsupported-version":
		response.Protocol = "unsupported.connector.protocol"
	case "both":
		response.Result = json.RawMessage(`{}`)
		response.Error = &protocol.Error{Code: "bad", Message: "bad"}
	case "neither":
	case "null-result":
		response.Result = json.RawMessage(`null`)
	case "raw-result":
		response.Result = json.RawMessage(os.Getenv("RESULT"))
	case "publication-result":
		if request.Method == "describe" {
			response.Result = publicationDescriptionResult()
		} else {
			response.Result = json.RawMessage(os.Getenv("RESULT"))
		}
	case "native-runtime-paths":
		home, err := os.UserHomeDir()
		if err != nil {
			os.Exit(12)
		}
		for _, directory := range []string{"", os.Getenv("TEMP"), os.Getenv("TMP"), home} {
			file, err := os.CreateTemp(directory, "connector-child-")
			if err != nil {
				os.Exit(13)
			}
			if err := file.Close(); err != nil {
				os.Exit(14)
			}
		}
		response.Result = describeResult()
	case "malformed":
		_, _ = os.Stdout.WriteString("{")
		return
	case "invalid-utf8":
		_, _ = fmt.Fprintf(os.Stdout, `{"protocol":%q,"id":%q,"result":{"connector_id":"fake`, protocol.ProtocolVersion, request.ID)
		_, _ = os.Stdout.Write([]byte{0xff})
		_, _ = os.Stdout.WriteString(`connector","display_name":"Fake","protocol":"kata.connector.v1","capabilities":[],"account_identity":"account-1"}}`)
		return
	case "require-empty-settings":
		if string(request.Settings) != `{}` {
			response.Error = &protocol.Error{Code: "settings", Message: "settings must be an object"}
			break
		}
		response.Result = describeResult()
	case "trailing":
		response.Result = describeResult()
		_ = json.NewEncoder(os.Stdout).Encode(response)
		_, _ = os.Stdout.WriteString("{}")
		return
	case "oversized":
		response.Result = json.RawMessage(`"` + strings.Repeat("x", 4<<20) + `"`)
	case "structured-secret":
		response.Error = &protocol.Error{Code: "bad", Message: os.Getenv("TOKEN")}
	case "argument-secret":
		response.Error = &protocol.Error{
			Code:    "bad",
			Message: "configured --label=argument-redaction-value value argument-redaction-value",
		}
	case "encoded-structured-secret":
		encoded, err := json.Marshal(os.Getenv("TOKEN"))
		if err != nil {
			os.Exit(2)
		}
		response.Error = &protocol.Error{Code: "bad", Message: string(encoded)}
	case "solidus-escaped-environment-secret":
		if os.Getenv("TOKEN") != "a/b" {
			os.Exit(2)
		}
		response.Error = &protocol.Error{Code: "bad", Message: `"a\/b"`}
	case "html-unescaped-json-token":
		if os.Getenv("TOKEN") != "line\n\"quote\"\\<>&" {
			os.Exit(2)
		}
		response.Error = &protocol.Error{Code: "bad", Message: `"line\n\"quote\"\\<>&"`}
	case "embedded-encoded-environment-secret":
		if os.Getenv("TOKEN") != "line\n\"quote\"\\<>&/tail" {
			os.Exit(2)
		}
		response.Error = &protocol.Error{
			Code: `prefix line\n\"quote\"\\\u003c\u003e\u0026\u002ftail middle ` +
				`line\n\"quote\"\\\u003c\u003e\u0026\u002Ftail middle ` +
				`line\n\"quote\"\\\u003C\u003e\u0026\/tail suffix`,
			Message: `prefix line\n\"quote\"\\\u003c\u003e\u0026\/tail middle ` +
				`line\n\"quote\"\\\u003c>&\/tail middle ` +
				`line\n\"quote\"\\<>&\/tail suffix`,
		}
	case "unicode-solidus-json-tokens":
		if os.Getenv("TOKEN") != "a/b" {
			os.Exit(2)
		}
		response.Error = &protocol.Error{Code: `"a\u002fb"`, Message: `"a\u002Fb"`}
	case "delayed-structured-secret":
		_ = os.WriteFile(os.Getenv("SYNC"), []byte("ready"), 0o600) // #nosec G703 -- the test supplies a private temporary path.
		time.Sleep(100 * time.Millisecond)
		response.Error = &protocol.Error{Code: "bad", Message: os.Getenv("TOKEN")}
	case "settings-secret":
		response.Error = &protocol.Error{Code: "bad", Message: string(request.Settings)}
	case "solidus-escaped-settings-secret":
		if string(request.Settings) != `{"token":"a/b"}` {
			os.Exit(2)
		}
		response.Error = &protocol.Error{Code: "bad", Message: `"a\/b"`}
	default:
		response.Result = describeResult()
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
}

func observedEnvironment() []string {
	keys := []string{"TOKEN", "CONNECTOR_SECRET", "UNRELATED_SECRET", "MODE", "RESULT", "SYNC"}
	observed := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := os.LookupEnv(key); ok {
			observed = append(observed, key)
		}
	}
	return observed
}

func expectedEnvironment(mode string) []string {
	if mode == "structured-secret" || mode == "encoded-structured-secret" || mode == "solidus-escaped-environment-secret" || mode == "html-unescaped-json-token" || mode == "embedded-encoded-environment-secret" || mode == "unicode-solidus-json-tokens" {
		return []string{"TOKEN", "MODE"}
	}
	if mode == "delayed-structured-secret" {
		return []string{"TOKEN", "MODE", "SYNC"}
	}
	if mode == "raw-result" || mode == "publication-result" {
		return []string{"MODE", "RESULT"}
	}
	if mode == "spawn-stdout-holder" || mode == "spawn-stdout-holder-and-exit" || mode == "spawn-background-holder" || mode == "launch-marker" {
		return []string{"MODE", "SYNC"}
	}
	if mode != "" {
		return []string{"MODE"}
	}
	return []string{"TOKEN"}
}

func waitForProcessClientHelperPID(t *testing.T, readyPath string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(readyPath) // #nosec G304 -- test-owned temporary path.
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			require.NoError(t, err)
			return pid
		}
		require.ErrorIs(t, err, fs.ErrNotExist)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connector helper did not signal readiness")
	return 0
}

func awaitBoundedProcessClientError(t *testing.T, errCh <-chan error, descendantPID int) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		killProcessClientHelper(descendantPID)
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Fatal("connector call remained blocked after descendant cleanup")
		}
		t.Fatal("connector call exceeded its bounded cleanup period")
		return nil
	}
}

func killProcessClientHelper(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
	_ = process.Release()
}

func describeResult() json.RawMessage {
	b, err := json.Marshal(protocol.Description{
		ConnectorID: "fake.connector", DisplayName: "Fake", Protocol: protocol.ProtocolVersion,
		Capabilities: []protocol.Capability{}, AccountIdentity: "account-1", ConfigSchema: mustJSON(map[string]any{"type": "object"}),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func publicationDescriptionResult() json.RawMessage {
	b, err := json.Marshal(protocol.Description{
		ConnectorID: "fake.connector", DisplayName: "Fake", Protocol: protocol.ProtocolVersion,
		Capabilities: []protocol.Capability{protocol.CapabilityPublishComment}, SelfActorID: "actor-self",
		AccountIdentity: "account-1", ConfigSchema: mustJSON(map[string]any{"type": "object"}),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
