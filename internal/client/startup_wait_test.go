package client

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestAutoStartWaitsForSQLiteMigrationBeyondReadinessBudget(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	originalStart, originalDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	t.Cleanup(func() { startDetachedDaemonForEnsure, discoverDaemonForAutoStart = originalStart, originalDiscover })
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error { return nil }
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			if time.Since(start) < 45*time.Second {
				return ensureDiscovery{}, nil
			}
			return compatibleEnsureDiscovery("daemon.example:27123"), nil
		}
		target, err := autoStart(t.Context(), home)
		require.NoError(t, err)
		require.Equal(t, "http://daemon.example:27123", target.BaseURL)
	})
}

func TestAutoStartReportsChildExitAfterReadinessGrace(t *testing.T) {
	home := setupKataEnv(t)
	originalStart, originalDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	t.Cleanup(func() { startDetachedDaemonForEnsure, discoverDaemonForAutoStart = originalStart, originalDiscover })
	startDetachedDaemonForEnsure = func(_ context.Context, opts kitdaemon.StartDetachedOptions) error {
		if opts.Exited != nil {
			opts.Exited(errors.New("fixture startup failure"))
		}
		return nil
	}
	synctest.Test(t, func(t *testing.T) {
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) { return ensureDiscovery{}, nil }
		start := time.Now()
		_, err := autoStart(t.Context(), home)
		require.ErrorContains(t, err, "fixture startup failure")
		require.Equal(t, 5*time.Second, time.Since(start))
	})
}

func TestAutoStartWaitsForConcurrentCutoverOwnerAfterChildExits(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	lock := flock.New(filepath.Join(home, "kata.db.import.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	originalStart, originalDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	t.Cleanup(func() { startDetachedDaemonForEnsure, discoverDaemonForAutoStart = originalStart, originalDiscover })
	startDetachedDaemonForEnsure = func(_ context.Context, opts kitdaemon.StartDetachedOptions) error {
		if opts.Exited != nil {
			opts.Exited(errors.New("another process owns cutover"))
		}
		return nil
	}
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			if time.Since(started) < 6*time.Second {
				return ensureDiscovery{}, nil
			}
			return compatibleEnsureDiscovery("daemon.example:27123"), nil
		}
		target, err := autoStart(t.Context(), home)
		require.NoError(t, err)
		require.Equal(t, "http://daemon.example:27123", target.BaseURL)
	})
}

func TestWaitForDaemonStartupBoundsExitAfterLateCutoverLockObservation(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	exited := make(chan error, 1)
	exited <- errors.New("retry child exited during cutover")
	probeCalls := 0
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		err := waitForDaemonStartupWithProbe(ctx, 5*time.Second, exited, func(context.Context) (bool, error) {
			return false, nil
		}, true, func(context.Context) bool {
			probeCalls++
			return probeCalls == 2
		})
		require.ErrorContains(t, err, "concurrent migration has stopped")
	})
}

func TestWaitForDaemonStartupRestartsGraceAfterCutoverLockReacquisition(t *testing.T) {
	exited := make(chan error, 1)
	exited <- errors.New("another process owns cutover")
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		locked := true
		err := waitForDaemonStartupWithProbe(t.Context(), 5*time.Second, exited, func(context.Context) (bool, error) {
			elapsed := time.Since(started)
			if elapsed >= 4*time.Second {
				locked = false
			}
			if elapsed >= 6*time.Second {
				locked = true
			}
			if elapsed >= 8*time.Second {
				locked = false
			}
			return elapsed >= 12*time.Second, nil
		}, true, func(context.Context) bool { return locked })
		require.NoError(t, err)
		require.Equal(t, 12*time.Second, time.Since(started))
	})
}

func TestWaitForDaemonStartupAllowsCurrentSchemaConcurrentStarterGrace(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	require.NoError(t, source.Close())

	for _, test := range []struct {
		name      string
		readyAt   time.Duration
		wantError bool
	}{
		{name: "winner becomes ready", readyAt: 100 * time.Millisecond},
		{name: "no winner preserves exit error", readyAt: 20 * time.Second, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exited := make(chan error, 1)
			exited <- errors.New("fixture listener bind conflict")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				started := time.Now()
				err := WaitForDaemonStartup(ctx, 5*time.Second, exited, func(context.Context) (bool, error) {
					return time.Since(started) >= test.readyAt, nil
				})
				if test.wantError {
					require.ErrorContains(t, err, "fixture listener bind conflict")
					require.Equal(t, 5*time.Second, time.Since(started))
					return
				}
				require.NoError(t, err)
				require.Equal(t, test.readyAt, time.Since(started))
			})
		})
	}
}

func TestWaitForDaemonStartupReturnsChildExitForOldSchemaWithoutCutover(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	source, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	exited := make(chan error, 1)
	exited <- errors.New("fixture preflight failure")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		started := time.Now()
		err := WaitForDaemonStartup(ctx, time.Second, exited, func(context.Context) (bool, error) {
			return false, nil
		})
		require.ErrorContains(t, err, "fixture preflight failure")
		require.Equal(t, daemonChildExitGrace, time.Since(started))
	})
}

func TestMigrationPendingUsesCutoverLockDuringSourceSwap(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	// The source pathname can briefly be absent while the lock owner installs
	// its replacement. Readiness polling must neither open it nor report failure.
	lock := flock.New(filepath.Join(home, "kata.db.import.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	require.True(t, sqliteMigrationPending(t.Context()))
	require.NoFileExists(t, filepath.Join(home, "kata.db"))
}

func TestMigrationPendingKeepsCutoverLockThroughSchemaPeek(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	path := filepath.Join(home, "kata.db")
	source, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	peekCalled, cutoverLockAcquiredDuringPeek := false, false
	pending := sqliteMigrationPendingForPath(t.Context(), path, func(ctx context.Context, path string) (int, error) {
		peekCalled = true
		competingLock := flock.New(path + ".import.lock")
		acquired, err := competingLock.TryLock()
		require.NoError(t, err)
		cutoverLockAcquiredDuringPeek = acquired
		defer func() {
			if acquired {
				require.NoError(t, competingLock.Unlock())
			}
			require.NoError(t, competingLock.Close())
		}()
		return sqlitestore.PeekSchemaVersion(ctx, path)
	})

	require.True(t, pending)
	require.True(t, peekCalled)
	require.False(t, cutoverLockAcquiredDuringPeek, "schema peek must run while the cutover lock is held")
}

func TestAutoStartWaitsForReadinessAfterCutoverUnlock(t *testing.T) {
	for _, unlockAfter := range []time.Duration{0, 2 * time.Second} {
		t.Run(unlockAfter.String(), func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			lock := flock.New(filepath.Join(home, "kata.db.import.lock"))
			require.NoError(t, lock.Lock())
			t.Cleanup(func() { require.NoError(t, lock.Close()) })
			exited := make(chan error, 1)
			exited <- errors.New("another process owns cutover")
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				unlocked := false
				err := WaitForDaemonStartup(t.Context(), 5*time.Second, exited, func(context.Context) (bool, error) {
					if !unlocked && time.Since(started) >= unlockAfter {
						require.NoError(t, lock.Unlock())
						unlocked = true
					}
					// The winner still opens storage and publishes its listener
					// after releasing the cutover lock.
					return time.Since(started) >= unlockAfter+4*time.Second, nil
				})
				require.NoError(t, err)
			})
		})
	}
}

func TestAutoStartProfileIgnoresDefaultHomeCutover(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	lock := flock.New(filepath.Join(home, "kata.db.import.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	profileHome := t.TempDir()
	originalStart, originalDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	t.Cleanup(func() { startDetachedDaemonForEnsure, discoverDaemonForAutoStart = originalStart, originalDiscover })
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error { return nil }
	discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) { return ensureDiscovery{}, nil }
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		started := time.Now()
		_, err := autoStartWithEnvironment(ctx, profileHome, []string{
			"KATA_HOME=" + profileHome,
			"KATA_DSN=" + filepath.Join(profileHome, "kata.db"),
		})
		require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
		require.Equal(t, daemonStartupWait, time.Since(started))
	})
}

func TestAutoStartWaitsAfterOwnMigrationUnlock(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	lock := flock.New(filepath.Join(home, "kata.db.import.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		unlocked := false
		err := WaitForDaemonStartup(t.Context(), 5*time.Second, nil, func(context.Context) (bool, error) {
			if !unlocked && time.Since(started) >= 4*time.Second {
				require.NoError(t, lock.Unlock())
				unlocked = true
			}
			return time.Since(started) >= 6*time.Second, nil
		})
		require.NoError(t, err)
	})
}
