package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestEnvHTTPTimeout(t *testing.T) {
	const def = 5 * time.Second

	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "empty returns default", env: "", want: def},
		{name: "valid override", env: "30s", want: 30 * time.Second},
		{name: "minutes parse", env: "2m", want: 2 * time.Minute},
		{name: "garbage falls back", env: "not-a-duration", want: def},
		{name: "zero falls back", env: "0s", want: def},
		{name: "negative falls back", env: "-10s", want: def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertEnvDurationOverride(t, "KATA_HTTP_TIMEOUT", tc.env, def, tc.want, envHTTPTimeout)
		})
	}
}

func TestLongRunningClientForLeavesResponseHeadersUnbounded(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())

	c, err := longRunningClientFor(context.Background(), "http://127.0.0.1:7373")
	if err != nil {
		t.Fatalf("longRunningClientFor: %v", err)
	}

	if c.Timeout != 0 {
		t.Fatalf("long-running client timeout = %v, want no overall timeout", c.Timeout)
	}
	if tr, ok := c.Transport.(*http.Transport); ok && tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("response header timeout = %v, want no response-header cap", tr.ResponseHeaderTimeout)
	}
}

func TestEnsureDaemon_RemoteUnavailableMapsToCLIError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1") // closed port

	_, err := ensureDaemon(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ce *cliError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *cliError, got %T (%v)", err, err)
	}
	if ce.Kind != kindDaemonUnavail {
		t.Errorf("expected Kind=%v, got %v", kindDaemonUnavail, ce.Kind)
	}
	if ce.ExitCode != ExitDaemonUnavail {
		t.Errorf("expected ExitCode=%d, got %d", ExitDaemonUnavail, ce.ExitCode)
	}
}

func assertEnvDurationOverride(t *testing.T, envKey, envVal string, fallback, want time.Duration, parseFn func(time.Duration) time.Duration) {
	t.Helper()
	t.Setenv(envKey, envVal)
	got := parseFn(fallback)
	if got != want {
		t.Fatalf("%s=%q override failed: got %v, want %v", envKey, envVal, got, want)
	}
}
