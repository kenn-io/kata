//go:build federation_stress && !windows

package e2e_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestFederationStressHarnessBootsHubAndThreeSpokes(t *testing.T) {
	fx := newFederationStressFixture(t, 3)
	fx.enableProject(t, "stress")
	fx.waitForAllSpokes(t)
	fx.assertAllFoldedProjectionsMatch(t)
}

func TestFederationStressRandomizedWorkload(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		fx := newFederationStressFixture(rt, 3)
		fx.enableProject(rt, "stress")
		steps := rapid.IntRange(6, 12).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			fx.applyRandomOperation(rt)
			fx.waitForConvergence(rt)
			fx.assertNoDuplicateLiveClaims(rt)
		}
		fx.waitForConvergence(rt)
		fx.assertNoDuplicateLiveClaims(rt)
	})
}

func TestFederationStressWaitForPing(t *testing.T) {
	t.Run("retries_until_ping_ok", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/ping" {
				http.NotFound(w, r)
				return
			}
			if attempts.Add(1) < 4 {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(srv.Close)

		rec := &stressTBRecorder{federationStressTB: t}
		stressWaitForPing(rec, srv.URL, 5*time.Second, nil, &safeBuffer{})
		require.Empty(t, rec.failures)
		assert.GreaterOrEqual(t, attempts.Load(), int32(4))
	})

	t.Run("fails_fast_when_daemon_exits", func(t *testing.T) {
		port := federationStressFreeTCPPort(t)
		done := make(chan struct{})
		proc := &federationStressProcess{
			done: done,
			err:  errors.New("exit status 1"),
		}
		stderr := &safeBuffer{}
		_, err := stderr.Write([]byte("kata: bind: address already in use\n"))
		require.NoError(t, err)
		close(done)

		rec := &stressTBRecorder{federationStressTB: t}
		started := time.Now()
		stressWaitForPing(rec, "http://127.0.0.1:"+strconv.Itoa(port), 5*time.Second, proc, stderr)
		elapsed := time.Since(started)

		assert.Less(t, elapsed, 2*time.Second, "exited daemon should fail without waiting for the startup timeout")
		require.Len(t, rec.failures, 1)
		assert.Contains(t, rec.failures[0], "exit status 1")
		assert.Contains(t, rec.failures[0], "address already in use")
	})

	t.Run("reports_last_probe_and_stderr_on_timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		stderr := &safeBuffer{}
		_, err := stderr.Write([]byte("still starting\n"))
		require.NoError(t, err)

		rec := &stressTBRecorder{federationStressTB: t}
		stressWaitForPing(rec, srv.URL, time.Second, nil, stderr)

		require.Len(t, rec.failures, 1)
		assert.Contains(t, rec.failures[0], "503")
		assert.Contains(t, rec.failures[0], "still starting")
	})
}
