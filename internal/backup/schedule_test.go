package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	tick  chan time.Time
	armed chan struct{}
}

func (f *fakeClock) Now() time.Time                       { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fakeClock) After(time.Duration) <-chan time.Time { f.armed <- struct{}{}; return f.tick }
func (f *fakeClock) advance(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-f.armed:
	case <-time.After(5 * time.Second):
		t.Fatal("schedule did not arm interval")
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	f.mu.Unlock()
	f.tick <- now
}
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), tick: make(chan time.Time), armed: make(chan struct{}, 1)}
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backup")
		var zero T
		return zero
	}
}

// Contract: immediate consistent export, serial intervals, retention scoped to
// this database's owned regular files, and cancellation stops future exports.
func TestScheduleIntervalsRetentionAndIsolation(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "012345abcdef")
	other := filepath.Join(root, "fedcba543210")
	require.NoError(t, os.MkdirAll(own, 0o700))
	require.NoError(t, os.MkdirAll(other, 0o700))
	old := "kata-backup-20260901T000000.000000000Z-abcdef.jsonl"
	for _, path := range []string{filepath.Join(own, old), filepath.Join(other, old), filepath.Join(own, "notes.jsonl")} {
		require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	}
	link := filepath.Join(own, "kata-backup-20260901T000000.000000000Z-123456.jsonl")
	linkCreated := os.Symlink(filepath.Join(other, old), link) == nil
	exports := make(chan string, 4)
	schedule, err := New(Settings{Dir: root, StorageID: "012345abcdef", Interval: time.Hour, Retain: 24 * time.Hour, Export: func(_ context.Context, path string) error {
		require.NoError(t, os.WriteFile(path, []byte("snapshot"), 0o600))
		exports <- path
		return nil
	}})
	require.NoError(t, err)
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { schedule.Run(ctx, clock, func(err error) { t.Error(err) }); close(done) }()
	first := receive(t, exports)
	clock.advance(t, time.Hour)
	second := receive(t, exports)
	require.NotEqual(t, first, second)
	clock.advance(t, 25*time.Hour)
	third := receive(t, exports)
	select {
	case <-clock.armed:
	case <-time.After(5 * time.Second):
		t.Fatal("schedule did not finish retention")
	}
	for _, path := range []string{filepath.Join(own, old), first, second} {
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), path)
	}
	for _, path := range []string{filepath.Join(other, old), filepath.Join(own, "notes.jsonl"), third} {
		_, err := os.Stat(path)
		require.NoError(t, err)
	}
	if linkCreated {
		info, err := os.Lstat(link)
		require.NoError(t, err)
		require.NotZero(t, info.Mode()&os.ModeSymlink)
	}
	cancel()
	receive(t, done)
	select {
	case <-exports:
		t.Fatal("export after cancellation")
	default:
	}
}

func TestScheduleRejectsInvalidSettings(t *testing.T) {
	for _, settings := range []Settings{
		{Dir: t.TempDir(), StorageID: "../outside", Interval: time.Hour, Retain: time.Hour, Export: func(context.Context, string) error { return nil }},
		{Dir: t.TempDir(), StorageID: "012345abcdef", Interval: 0, Retain: time.Hour, Export: func(context.Context, string) error { return nil }},
		{Dir: t.TempDir(), StorageID: "012345abcdef", Interval: time.Hour, Retain: 0, Export: func(context.Context, string) error { return nil }},
		{Dir: t.TempDir(), StorageID: "012345abcdef", Interval: time.Hour, Retain: time.Hour},
	} {
		_, err := New(settings)
		require.Error(t, err)
	}
}

func TestScheduleExportFailureKeepsOldBackupAndRetries(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "012345abcdef")
	require.NoError(t, os.MkdirAll(own, 0o700))
	old := filepath.Join(own, "kata-backup-20260901T000000.000000000Z-abcdef.jsonl")
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o600))
	attempts := 0
	reported := make(chan error, 1)
	exports := make(chan string, 1)
	schedule, err := New(Settings{Dir: root, StorageID: "012345abcdef", Interval: time.Hour, Retain: 24 * time.Hour, Export: func(_ context.Context, path string) error {
		attempts++
		if attempts == 1 {
			return errors.New("export unavailable")
		}
		require.NoError(t, os.WriteFile(path, []byte("snapshot"), 0o600))
		exports <- path
		return nil
	}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	clock := newFakeClock()
	go func() { schedule.Run(ctx, clock, func(err error) { reported <- err }); close(done) }()
	require.ErrorContains(t, receive(t, reported), "export unavailable")
	_, err = os.Stat(old)
	require.NoError(t, err)
	clock.advance(t, time.Hour)
	receive(t, exports)
	cancel()
	receive(t, done)
}
