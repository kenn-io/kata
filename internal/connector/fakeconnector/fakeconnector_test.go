package fakeconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/connector"
)

func TestPublishCommentRetryAfterCrashCreatesOneComment(t *testing.T) {
	fixture := &conformanceFixture{path: filepath.Join(t.TempDir(), "state.json")}
	require.NoError(t, fixture.Reset(t.Context()))
	require.NoError(t, Update(fixture.path, func(current *State) error {
		if current.Behavior.CrashAfterMutation == nil {
			current.Behavior.CrashAfterMutation = make(map[string]int)
		}
		current.Behavior.CrashAfterMutation["publish_comment"] = 1
		return nil
	}))
	request, err := json.Marshal(connector.Request{
		Protocol: connector.ProtocolVersion,
		ID:       "request-1",
		Method:   "publish_comment",
		Instance: "example-instance",
		Settings: json.RawMessage(`{}`),
		Params:   json.RawMessage(`{"root_key":"root-example","body":"publish once","operation_id":"publication-1"}`),
	})
	require.NoError(t, err)

	assert.Equal(t, exitCrashAfterMutation, Run(fixture.path, bytes.NewReader(request), &bytes.Buffer{}))
	assert.Zero(t, Run(fixture.path, bytes.NewReader(request), &bytes.Buffer{}))
	state, err := Load(fixture.path)
	require.NoError(t, err)
	require.Len(t, state.Roots, 1)
	assert.Len(t, state.Roots[0].Comments, 5)
	assert.Len(t, state.Mutations, 1)
	assert.Equal(t, 2, len(state.Calls))
}

func TestStateLockHonorsContextWhileOwnerIsLive(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.lock")
	release, err := lockContext(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	secondRelease, err := lockContext(ctx, lockPath)
	if secondRelease != nil {
		secondRelease()
		t.Fatal("second owner acquired a live lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second owner error = %v, want context deadline", err)
	}
}

func TestStateUpdateDoesNotStealLiveLockAndRecoversAfterKill(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	readyPath := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(statePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestKilledStateUpdateProbe$") // #nosec G204,G702 -- the current test executable is fixed by the Go test runner.
	command.Env = append(os.Environ(), "KATA_FAKE_STATE_PROBE="+statePath, "KATA_FAKE_READY_PROBE="+readyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state update probe did not acquire its lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(350 * time.Millisecond)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- Update(statePath, func(current *State) error {
			current.NextComment = 2
			return nil
		})
	}()
	var enteredWhileOwned bool
	select {
	case err := <-secondResult:
		enteredWhileOwned = true
		if err != nil {
			t.Fatalf("second update failed while testing live ownership: %v", err)
		}
	case <-time.After(350 * time.Millisecond):
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed state update exited successfully")
	}

	if enteredWhileOwned {
		t.Fatal("second update entered while the live owner still held the lock")
	}
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("waiting update did not acquire after owner death: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting update did not acquire promptly after owner death")
	}
	current, err := Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if current.NextComment != 2 {
		t.Fatalf("waiting update result = %d, want 2", current.NextComment)
	}
}

func TestKilledStateUpdateProbe(t *testing.T) {
	statePath := os.Getenv("KATA_FAKE_STATE_PROBE")
	readyPath := os.Getenv("KATA_FAKE_READY_PROBE")
	if statePath == "" || readyPath == "" {
		t.Skip("subprocess probe")
	}
	if err := Update(statePath, func(*State) error {
		if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil { // #nosec G703 -- readyPath is a test-owned subprocess synchronization file.
			return err
		}
		<-time.After(24 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
