package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

// A parent-controlled gate keeps the current-schema timeout child alive until
// launch returns. The other modes exercise a delayed schema-28 cutover.
func TestDetachedStartSurvivesReadinessTimeout(t *testing.T) {
	for _, mode := range []string{"readiness-timeout", "caller-cancel", "foreign-runtime"} {
		t.Run(mode, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_STARTUP_FIXTURE", "1")
			t.Setenv("KATA_STARTUP_FIXTURE_MODE", mode)
			childFinished := false
			t.Cleanup(func() {
				if !childFinished {
					stopStartupFixture(t, home)
				}
			})
			for _, key := range []string{"KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_AUTH_TOKEN_FILE", "PORT"} {
				t.Setenv(key, "")
			}
			childWaiting := make(chan struct{}, 1)
			childGate := make(chan struct{})
			releaseChild := sync.OnceFunc(func() { close(childGate) })
			cutoverComplete := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/release-child" {
					childWaiting <- struct{}{}
					select {
					case <-childGate:
						writer.WriteHeader(http.StatusNoContent)
					case <-request.Context().Done():
					}
					return
				}
				if request.URL.Path != "/cutover-complete" {
					http.NotFound(writer, request)
					return
				}
				select {
				case cutoverComplete <- struct{}{}:
				default:
				}
				writer.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(func() {
				releaseChild()
				server.Close()
			})
			t.Setenv("KATA_STARTUP_FIXTURE_NOTIFY_URL", server.URL)
			source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
			require.NoError(t, err)
			if mode != "readiness-timeout" {
				_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
				require.NoError(t, err)
			}
			require.NoError(t, source.Close())

			// Re-execute a copy without the .test suffix: the production launcher
			// intentionally refuses to detach ephemeral Go test executables.
			exeSource, err := os.Executable()
			require.NoError(t, err)
			body, err := os.ReadFile(exeSource) //nolint:gosec // Copies the current test executable into an isolated detached fixture.
			require.NoError(t, err)
			exe := filepath.Join(t.TempDir(), "kata-startup-fixture")
			if runtime.GOOS == "windows" {
				exe += ".exe"
			}
			require.NoError(t, os.WriteFile(exe, body, 0o700)) //nolint:gosec // the copied test executable must be executable
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDetachedStartFixture$") //nolint:gosec // executable is copied from this test binary into t.TempDir
			cmd.Env = os.Environ()
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			if mode == "readiness-timeout" {
				select {
				case <-childWaiting:
				case <-ctx.Done():
					t.Fatalf("child did not reach the release gate: %s", output)
				}
				_, err := os.Stat(filepath.Join(home, "cutover-complete"))
				require.ErrorIs(t, err, os.ErrNotExist, "child must still be gated when launch times out")
			}
			releaseChild()
			completionDeadline := time.NewTimer(30 * time.Second)
			defer completionDeadline.Stop()
			select {
			case <-cutoverComplete:
				childFinished = true
			case <-completionDeadline.C:
				t.Fatalf("detached child did not complete cutover: %s", output)
			}
			_, cutoverErr := os.Stat(filepath.Join(home, "cutover-complete"))
			require.NoError(t, cutoverErr, "detached child did not complete cutover: %s", output)
		})
	}
}

// A replaced daemon can hold the store owner lock through its shutdown drain,
// longer than the ordinary readiness budget. Detached start must keep waiting.
func TestDetachedStartWaitsForStoreOwner(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	for _, key := range []string{"KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_AUTH_TOKEN_FILE", "PORT"} {
		t.Setenv(key, "")
	}
	t.Cleanup(func() { stopStartupFixture(t, home) })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, copyStartupFixtureExecutable(t), "-test.run=^TestDetachedStartFixture$") //nolint:gosec // executable is copied from this test binary into t.TempDir
	cmd.Env = append(os.Environ(), "KATA_STARTUP_FIXTURE=1", "KATA_STARTUP_FIXTURE_MODE=owner-drain")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

// copyStartupFixtureExecutable copies this test binary without the .test
// suffix: the production launcher refuses to detach ephemeral Go test executables.
func copyStartupFixtureExecutable(t *testing.T) string {
	t.Helper()
	exeSource, err := os.Executable()
	require.NoError(t, err)
	body, err := os.ReadFile(exeSource) //nolint:gosec // Copies the current test executable into an isolated detached fixture.
	require.NoError(t, err)
	exe := filepath.Join(t.TempDir(), "kata-startup-fixture")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	require.NoError(t, os.WriteFile(exe, body, 0o700)) //nolint:gosec // the copied test executable must be executable
	return exe
}

func TestDetachedStartRetryJoinsConcurrentCutoverOwner(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	dbPath := filepath.Join(home, "kata.db")
	source, err := sqlitestore.Open(t.Context(), dbPath)
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(ns.DataDir, 0o700)) //nolint:gosec // the runtime fixture is under the test's temporary KATA_HOME
	lock := flock.New(dbPath + ".import.lock")
	require.NoError(t, lock.Lock())
	lockHeld := true
	t.Cleanup(func() {
		if lockHeld {
			require.NoError(t, lock.Unlock())
		}
		require.NoError(t, lock.Close())
	})

	concurrentPID := startSleepProcess(t).Process.Pid
	const listen = "127.0.0.1:17777"
	retryExited := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/retry-exited" {
			http.NotFound(writer, request)
			return
		}
		select {
		case retryExited <- struct{}{}:
		default:
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	exeSource, err := os.Executable()
	require.NoError(t, err)
	body, err := os.ReadFile(exeSource) //nolint:gosec // Copies the current test executable into an isolated detached fixture.
	require.NoError(t, err)
	exe := filepath.Join(t.TempDir(), "kata-startup-fixture")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	require.NoError(t, os.WriteFile(exe, body, 0o700)) //nolint:gosec // the copied test executable must be executable

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDetachedStartFixture$") //nolint:gosec // executable is a test fixture copied into t.TempDir
	cmd.Env = append(os.Environ(),
		"KATA_STARTUP_FIXTURE=1",
		"KATA_STARTUP_FIXTURE_MODE=retry-wrapper",
		"KATA_STARTUP_FIXTURE_NOTIFY_URL="+server.URL,
		"KATA_STARTUP_FIXTURE_LISTEN="+listen,
		"KATA_STARTUP_EXPECTED_PID="+strconv.Itoa(concurrentPID),
	)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-done
		}
	})

	select {
	case <-retryExited:
	case err := <-done:
		t.Fatalf("retry fixture exited before reporting lock contention: %v\n%s", err, output.String())
	case <-ctx.Done():
		t.Fatalf("retry child did not report lock contention: %v\n%s", ctx.Err(), output.String())
	}

	require.NoError(t, lock.Unlock())
	lockHeld = false
	require.NoError(t, jsonl.AutoCutover(ctx, dbPath))
	identity, ok := kitdaemon.ReadProcessIdentity(concurrentPID)
	require.True(t, ok)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:               concurrentPID,
		ProcessIdentityV2: identity,
		Network:           kitdaemon.NetworkTCP,
		Address:           listen,
		Metadata:          map[string]string{"db_path": dbPath},
		StartedAt:         time.Now().UTC(),
	})
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err, "%s", output.String())
	case <-ctx.Done():
		t.Fatalf("retry did not adopt the compatible daemon after cutover: %v\n%s", ctx.Err(), output.String())
	}
}

func TestDetachedStartFixture(t *testing.T) {
	if os.Getenv("KATA_STARTUP_FIXTURE") != "1" {
		t.Skip("subprocess fixture")
	}
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(ns.DataDir, 0o700)) //nolint:gosec // the runtime fixture is under the test's temporary KATA_HOME
	nsDir := ns.DataDir
	ctx := context.Background()
	mode := os.Getenv("KATA_STARTUP_FIXTURE_MODE")
	if mode == "retry-wrapper" {
		t.Setenv("KATA_STARTUP_FIXTURE_MODE", "retry-child")
		result, err := defaultLaunchDetachedDaemon(
			t.Context(), nsDir, os.Getenv("KATA_STARTUP_FIXTURE_LISTEN"), false, true,
		)
		require.NoError(t, err)
		wantPID, err := strconv.Atoi(os.Getenv("KATA_STARTUP_EXPECTED_PID"))
		require.NoError(t, err)
		require.Equal(t, wantPID, result.PID)
		require.Equal(t, "already_running", result.Action)
		return
	}
	if mode == "readiness-timeout" {
		// No owner holds the store, so only the ordinary readiness budget applies.
		daemonOwnerLockWait = 0
	}
	if mode == "caller-cancel" || mode == "foreign-runtime" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Second)
		defer cancel()
	}
	listen := ""
	if mode == "foreign-runtime" {
		identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
		require.True(t, ok)
		_, err := (kitdaemon.RuntimeStore{Dir: nsDir}).Write(kitdaemon.RuntimeRecord{
			PID: os.Getpid(), ProcessIdentityV2: identity,
			Network: "tcp", Address: "127.0.0.1:17777",
		})
		require.NoError(t, err)
		listen = "127.0.0.1:17778"
	}
	if mode == "owner-drain" {
		dsn, err := config.KataDSN(t.Context())
		require.NoError(t, err)
		release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), daemonruntime.StoreOwnerLockPaths(nsDir, dsn))
		require.NoError(t, err)
		// The contract is about wall time: ownership outlasts the 5s ordinary readiness budget.
		timer := time.AfterFunc(6*time.Second, release)
		t.Cleanup(func() {
			if timer.Stop() {
				release()
			}
		})
	}
	result, err := defaultLaunchDetachedDaemon(ctx, nsDir, listen, false, true)
	if mode == "owner-drain" {
		require.NoError(t, err)
		require.Equal(t, "started", result.Action)
		return
	}
	if mode == "readiness-timeout" {
		var targetErr *cliError
		require.ErrorAs(t, err, &targetErr)
		require.Equal(t, kindDaemonUnavail, targetErr.Kind)
		require.Equal(t, ExitDaemonUnavail, targetErr.ExitCode)
		require.ErrorContains(t, err, client.ErrLocalDaemonUnreachable.Error())
		require.ErrorContains(t, err, "daemon startup is still in progress after 5s")
		require.Empty(t, result.Action)
	}
	if mode == "foreign-runtime" {
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Empty(t, result.Action)
	}
	// Startup may still be pending, but its child must survive this caller.
	t.Logf("launch result: %v", err)
}

func runStartupFixture() {
	home := os.Getenv("KATA_HOME")
	dbPath := filepath.Join(home, "kata.db")
	if os.Getenv("KATA_STARTUP_FIXTURE_MODE") == "retry-child" {
		err := jsonl.AutoCutover(context.Background(), dbPath)
		if errors.Is(err, jsonl.ErrCutoverInProgress) {
			if notifyErr := notifyStartupFixture("retry-exited"); notifyErr != nil {
				_, _ = os.Stderr.WriteString(notifyErr.Error())
				os.Exit(1)
			}
		}
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	if !ok {
		_, _ = os.Stderr.WriteString("read detached fixture process identity")
		os.Exit(1)
	}
	pidRecord := strconv.Itoa(os.Getpid()) + "\n" + string(identity)
	if err := os.WriteFile(filepath.Join(home, "cutover-child.pid"), []byte(pidRecord), 0o600); err != nil { //nolint:gosec // fixture path under temporary KATA_HOME
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	if os.Getenv("KATA_STARTUP_FIXTURE_MODE") == "owner-drain" {
		newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
		// The parent test kills this daemon through cutover-child.pid.
		if err := runDaemonProcess(context.Background(), "", false, true, newDaemonRestart(os.Stderr)); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("KATA_STARTUP_FIXTURE_MODE") == "readiness-timeout" {
		if err := notifyStartupFixture("release-child"); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
	} else {
		timer := time.NewTimer(6 * time.Second)
		<-timer.C
	}
	err := jsonl.AutoCutover(context.Background(), dbPath)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(home, "cutover-complete"), nil, 0o600); err != nil { //nolint:gosec // fixture path under temporary KATA_HOME
		os.Exit(1)
	}
	if err := notifyStartupFixture("cutover-complete"); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func stopStartupFixture(t *testing.T, home string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, "cutover-child.pid")) //nolint:gosec // fixture path under temporary KATA_HOME
	if err != nil {
		return
	}
	fields := strings.Fields(string(body))
	if len(fields) != 2 {
		return
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return
	}
	identity, ok := kitdaemon.ReadProcessIdentity(pid)
	if !ok || string(identity) != fields[1] {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := process.Kill(); err != nil {
		if currentIdentity, ok := kitdaemon.ReadProcessIdentity(pid); ok && string(currentIdentity) == fields[1] {
			t.Errorf("stop detached cutover fixture process %d: %v", pid, err)
		}
		return
	}
	if err := waitForDaemonProcesses(context.Background(), []int{pid}, 5*time.Second); err != nil {
		t.Errorf("wait for detached cutover fixture process %d: %v", pid, err)
	}
}

func notifyStartupFixture(event string) error {
	baseURL := os.Getenv("KATA_STARTUP_FIXTURE_NOTIFY_URL")
	if baseURL == "" {
		return errors.New("startup fixture notification URL is missing")
	}
	response, err := http.Post(baseURL+"/"+event, "text/plain", nil) //nolint:gosec // fixture-only loopback URL supplied by the parent test
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		return errors.New("startup fixture notification failed")
	}
	return nil
}
