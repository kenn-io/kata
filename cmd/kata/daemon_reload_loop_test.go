package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/hooks"
)

// recordingDispatcher captures Reload calls so the SIGHUP loop test can
// observe what was dispatched without spawning a real *hooks.Dispatcher.
type recordingDispatcher struct {
	mu          sync.Mutex
	reloadCalls []hooks.LoadedConfig
}

func (r *recordingDispatcher) CurrentConfig() hooks.Config {
	return hooks.Config{
		PoolSize:             4,
		QueueCap:             1000,
		OutputDiskCap:        100 << 20,
		RunsLogMaxBytes:      50 << 20,
		RunsLogKeep:          5,
		QueueFullLogInterval: 60 * time.Second,
	}
}

func (r *recordingDispatcher) Reload(lc hooks.LoadedConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloadCalls = append(r.reloadCalls, lc)
}

// nopLogger satisfies loopLogger without writing anything.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func TestRunReloadLoop_DispatchesOnSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := setupKataEnv(t)
		path := filepath.Join(dir, "hooks.toml")
		require.NoError(t, os.WriteFile(path, []byte(`[[hook]]
event = "issue.created"
command = "true"
`), 0o600))
		rec := &recordingDispatcher{}
		sigs := make(chan os.Signal, 1)
		ctx, cancel := context.WithCancel(t.Context())

		done := make(chan struct{})
		go func() {
			runReloadLoop(ctx, sigs, path, rec, nopLogger{})
			close(done)
		}()

		sigs <- os.Interrupt
		synctest.Wait()
		rec.mu.Lock()
		reloads := append([]hooks.LoadedConfig(nil), rec.reloadCalls...)
		rec.mu.Unlock()
		require.Len(t, reloads, 1)
		require.Len(t, reloads[0].Snapshot.Hooks, 1, "expected one hook in reloaded snapshot")
		cancel()
		synctest.Wait()
		<-done
	})
}
