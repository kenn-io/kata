package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"go.kenn.io/kata/internal/client"
	kitdaemon "go.kenn.io/kit/daemon"
)

const (
	defaultDaemonLivenessInterval         = 15 * time.Second
	defaultDaemonLivenessProbeTimeout     = 5 * time.Second
	defaultDaemonLivenessFailureThreshold = 3
	daemonLivenessStderrTimeout           = 250 * time.Millisecond
	daemonLivenessMaxResponseBytes        = 1 << 16
	daemonLivenessMaxStackBytes           = 16 << 20
)

// Package-level test seams so watchdog tests can run with short budgets and
// simulated wedged listeners without adding unused production flags.
var (
	daemonLivenessInterval         = defaultDaemonLivenessInterval
	daemonLivenessProbeTimeout     = defaultDaemonLivenessProbeTimeout
	daemonLivenessFailureThreshold = defaultDaemonLivenessFailureThreshold
	listenPrimaryDaemon            = kitdaemon.Listen
)

type daemonLivenessConfig struct {
	Endpoint         kitdaemon.Endpoint
	ExpectedPID      int
	Interval         time.Duration
	ProbeTimeout     time.Duration
	FailureThreshold int
	Diagnostics      io.Writer
	Probe            func(context.Context) error
	BeforeExit       func()
	Exit             func(int)
}

func startDaemonLivenessWatchdog(
	ctx context.Context,
	workers *daemonWorkerGroup,
	endpoint kitdaemon.Endpoint,
	runtimeFile string,
	diagnostics io.Writer,
) {
	cfg := daemonLivenessConfig{
		Endpoint:         endpoint,
		ExpectedPID:      os.Getpid(),
		Interval:         daemonLivenessInterval,
		ProbeTimeout:     daemonLivenessProbeTimeout,
		FailureThreshold: daemonLivenessFailureThreshold,
		Diagnostics:      diagnostics,
		BeforeExit: func() {
			if runtimeFile != "" {
				_ = os.Remove(runtimeFile)
			}
		},
		Exit: os.Exit,
	}
	workers.Go(func() {
		runDaemonLivenessWatchdog(ctx, cfg)
	})
}

func runDaemonLivenessWatchdog(ctx context.Context, cfg daemonLivenessConfig) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultDaemonLivenessInterval
	}
	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultDaemonLivenessProbeTimeout
	}
	failureThreshold := cfg.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = defaultDaemonLivenessFailureThreshold
	}
	expectedPID := cfg.ExpectedPID
	if expectedPID <= 0 {
		expectedPID = os.Getpid()
	}
	exitFn := cfg.Exit
	if exitFn == nil {
		exitFn = os.Exit
	}

	probe := cfg.Probe
	cleanup := func() {}
	if probe == nil {
		var err error
		probe, cleanup, err = newDaemonLivenessProbe(cfg.Endpoint, expectedPID)
		if err != nil {
			if cfg.Diagnostics != nil {
				_, _ = fmt.Fprintf(cfg.Diagnostics, "kata daemon: liveness watchdog disabled: %v\n", err)
			}
			return
		}
	}
	defer cleanup()

	// Wait one full interval between probes so wake-from-sleep or momentary
	// stalls cannot trigger back-to-back burst failures.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}

		probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
		err := probe(probeCtx)
		probeCancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			consecutiveFailures = 0
			timer.Reset(interval)
			continue
		}

		consecutiveFailures++
		if consecutiveFailures >= failureThreshold {
			report := formatDaemonLivenessTrip(
				cfg.Endpoint,
				expectedPID,
				consecutiveFailures,
				probeTimeout,
				interval,
				err,
				captureAllGoroutineStacks(),
			)
			writeDaemonLivenessReport(cfg.Diagnostics, report)
			if cfg.BeforeExit != nil {
				cfg.BeforeExit()
			}
			exitFn(ExitInternal)
			return
		}
		timer.Reset(interval)
	}
}

func newDaemonLivenessProbe(endpoint kitdaemon.Endpoint, expectedPID int) (func(context.Context) error, func(), error) {
	target, err := daemonMCPSelfEndpoint(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve liveness probe endpoint: %w", err)
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		// Force a fresh connection per probe so the self-check verifies the
		// listener accept loop as well as request handling.
		DisableKeepAlives: true,
		Proxy:             nil,
	}
	baseURL := "http://" + target.Address
	if target.Network == kitdaemon.NetworkUnix {
		baseURL = client.UnixBase
		socketPath := target.Address
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}
	} else {
		transport.DialContext = dialer.DialContext
	}

	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	probeURL := baseURL + "/api/v1/ping"

	probe := func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return err
		}
		// Omit X-Kata-Idle-Keepalive so observational probes do not prevent
		// auto-start idle shutdown.
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, daemonLivenessMaxResponseBytes))
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d from /api/v1/ping", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, daemonLivenessMaxResponseBytes))
		if err != nil {
			return fmt.Errorf("read /api/v1/ping response: %w", err)
		}
		var info client.PingInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return fmt.Errorf("decode /api/v1/ping response: %w", err)
		}
		if !info.OK || info.Service != "kata" {
			return fmt.Errorf("invalid /api/v1/ping response: ok=%t service=%q", info.OK, info.Service)
		}
		if expectedPID > 0 && info.PID != expectedPID {
			return fmt.Errorf("/api/v1/ping reported pid %d, expected %d", info.PID, expectedPID)
		}
		return nil
	}
	return probe, transport.CloseIdleConnections, nil
}

func captureAllGoroutineStacks() []byte {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= daemonLivenessMaxStackBytes {
			return buf[:n]
		}
		buf = make([]byte, min(len(buf)*2, daemonLivenessMaxStackBytes))
	}
}

func formatDaemonLivenessTrip(
	endpoint kitdaemon.Endpoint,
	pid int,
	failures int,
	probeTimeout time.Duration,
	interval time.Duration,
	lastErr error,
	stacks []byte,
) []byte {
	var buf bytes.Buffer
	_, _ = fmt.Fprintf(
		&buf,
		"kata daemon: liveness watchdog tripped: pid=%d endpoint=%s failed %d consecutive /api/v1/ping probes (timeout=%s, interval=%s); last error: %v\n",
		pid,
		endpoint.ConfigAddress(),
		failures,
		probeTimeout,
		interval,
		lastErr,
	)
	_, _ = buf.WriteString("kata daemon: dumping goroutine stacks and exiting so supervisor can restart\n")
	_, _ = buf.Write(stacks)
	if len(stacks) == 0 || stacks[len(stacks)-1] != '\n' {
		_ = buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func writeDaemonLivenessReport(diagnostics io.Writer, report []byte) {
	if diagnostics == nil {
		return
	}
	// When diagnostics mirrors to both daemon.log and stderr, persist and sync
	// daemon.log first, then bound stderr so a blocked pipe cannot prevent exit.
	if dw, ok := diagnostics.(daemonDiagnosticWriter); ok {
		writeAndSync(dw.file, report)
		writeBounded(dw.stderr, report, daemonLivenessStderrTimeout)
		return
	}
	if isRegularFileWriter(diagnostics) {
		writeAndSync(diagnostics, report)
		return
	}
	writeBounded(diagnostics, report, daemonLivenessStderrTimeout)
}

func writeAndSync(w io.Writer, data []byte) {
	if w == nil {
		return
	}
	_, _ = w.Write(data)
	if f, ok := w.(*os.File); ok {
		_ = f.Sync()
	}
}

func isRegularFileWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode().IsRegular()
}

func writeBounded(w io.Writer, data []byte, timeout time.Duration) {
	if w == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.Write(data)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
