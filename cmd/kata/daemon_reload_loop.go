package main

import (
	"context"
	"os"

	"go.kenn.io/kata/internal/hooks"
)

// reloadable is the small surface runReloadLoop needs from the
// dispatcher. Defined as an interface so tests can inject recorders
// without spinning up a real dispatcher + worker pool.
type reloadable interface {
	CurrentConfig() hooks.Config
	Reload(hooks.LoadedConfig)
}

// loopLogger is the printf-only logger interface used by runReloadLoop.
// *log.Logger satisfies this naturally.
type loopLogger interface {
	Printf(format string, args ...any)
}

// runReloadLoop blocks until ctx is done, calling LoadReload + Reload
// each time a signal arrives on sigs. Errors are logged; the previous
// snapshot is kept on parse failure (the dispatcher is never told to
// reload a config it can't apply).
func runReloadLoop(
	ctx context.Context,
	sigs <-chan os.Signal,
	configPath string,
	disp reloadable,
	lg loopLogger,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			loaded, err := hooks.LoadReload(configPath, disp.CurrentConfig())
			if err != nil {
				lg.Printf("hooks reload failed: %v (keeping previous config)", err)
				continue
			}
			disp.Reload(loaded)
		}
	}
}
