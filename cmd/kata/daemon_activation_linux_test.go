//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestSocketActivationInheritedProcess(t *testing.T) {
	if os.Getenv("KATA_ACTIVATION_TEST_CHILD") != "1" {
		return
	}
	os.Setenv("LISTEN_PID", fmt.Sprint(os.Getpid()))
	if os.Getenv("KATA_ACTIVATION_TEST_WRONG_PID") == "1" {
		os.Setenv("LISTEN_PID", "1")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	if err := runDaemonProcess(ctx, "", false, true, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSocketActivationInheritedAuthenticatedReadback(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, os.Chmod(home, 0o700))
	socket := filepath.Join(home, "manager-api.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	require.NoError(t, err)
	defer ln.Close()
	before, err := os.Stat(socket)
	require.NoError(t, err)
	file, err := ln.File()
	require.NoError(t, err)
	defer file.Close()
	tokenPath := filepath.Join(home, "manager.token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("activation-test-secret"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("socket_activation = \"systemd\"\n[auth]\ntoken_file = %q\n", tokenPath)), 0600))
	exe, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(context.WithoutCancel(t.Context()), exe, "-test.run=^TestSocketActivationInheritedProcess$")
	child.Env = append(os.Environ(), "KATA_ACTIVATION_TEST_CHILD=1", "LISTEN_FDS=1", "KATA_AUTH_TOKEN=", "KATA_AUTH_TOKEN_FILE=", "KATA_LISTEN=", "PORT=")
	child.ExtraFiles = []*os.File{file}
	var logs bytes.Buffer
	child.Stdout = &logs
	child.Stderr = &logs
	require.NoError(t, child.Start())
	done := make(chan error, 1)
	go func() { done <- child.Wait(); close(done) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-done })
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	var record kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		records, e := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
		if e != nil || len(records) != 1 {
			return false
		}
		record = records[0]
		return true
	}, 15*time.Second, 20*time.Millisecond, "daemon did not become ready")
	require.Equal(t, "unix://"+socket, record.Endpoint().ConfigAddress())
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: time.Second}
	request := func(method, path, body, token string) (int, string) {
		req, e := http.NewRequestWithContext(t.Context(), method, "http://kata.invalid"+path, strings.NewReader(body))
		require.NoError(t, e)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, e := hc.Do(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		data, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		return resp.StatusCode, string(data)
	}
	status, _ := request("POST", "/api/v1/projects", `{"name":"forbidden-project","actor":"operator"}`, "")
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := request("POST", "/api/v1/projects", `{"name":"activated-project","actor":"operator"}`, "activation-test-secret")
	require.Equal(t, http.StatusOK, status, body)
	status, body = request("GET", "/api/v1/projects", "", "activation-test-secret")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "activated-project")
	require.NotContains(t, body, "forbidden-project")
	require.NoError(t, child.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		require.NoError(t, err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("activated daemon did not exit after SIGTERM")
	}
	require.Eventually(t, func() bool {
		records, e := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
		return e == nil && len(records) == 0
	}, 10*time.Second, 20*time.Millisecond)
	after, err := os.Stat(socket)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "Kata must preserve the manager socket inode")
	// The manager's still-open listener must accept after Kata closes its copy.
	accepted := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			_ = c.Close()
		}
		accepted <- e
	}()
	c, err := net.DialTimeout("unix", socket, time.Second)
	require.NoError(t, err)
	_ = c.Close()
	require.NoError(t, <-accepted)
}

func TestSocketActivationInheritedInvalidDescriptors(t *testing.T) {
	for _, kind := range []string{"missing", "regular-file", "datagram", "multiple", "wrong-pid", "malformed-count"} {
		t.Run(kind, func(t *testing.T) {
			resetFlags(t)
			home := setupKataEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
			var files []*os.File
			count := "1"
			switch kind {
			case "missing":
				count = "0"
			case "regular-file":
				file, err := os.CreateTemp(home, "not-a-socket")
				require.NoError(t, err)
				defer file.Close()
				files = []*os.File{file}
			case "datagram":
				conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: activationSocketPath(t), Net: "unixgram"})
				require.NoError(t, err)
				defer conn.Close()
				file, err := conn.File()
				require.NoError(t, err)
				defer file.Close()
				files = []*os.File{file}
			default:
				ln, _ := activationListenerFixture(t)
				file, err := ln.File()
				require.NoError(t, err)
				defer file.Close()
				files = []*os.File{file}
				if kind == "multiple" {
					files = append(files, file)
					count = "2"
				}
				if kind == "malformed-count" {
					count = "not-a-count"
				}
			}
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestSocketActivationInheritedProcess$")
			child.Env = append(os.Environ(), "KATA_ACTIVATION_TEST_CHILD=1", "LISTEN_FDS="+count)
			if kind == "wrong-pid" {
				child.Env = append(child.Env, "KATA_ACTIVATION_TEST_WRONG_PID=1")
			}
			child.ExtraFiles = files
			output, err := child.CombinedOutput()
			require.Error(t, err, string(output))
			require.NoError(t, ctx.Err(), "invalid acquisition must fail without serving an own-bound fallback")
		})
	}
}
