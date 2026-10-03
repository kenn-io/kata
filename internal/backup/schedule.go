// Package backup schedules logical snapshots without a separate helper process.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/kit/safefileio"
)

const timestampLayout = "20060102T150405.000000000Z"

var storageIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
var ownedFilename = regexp.MustCompile(`^kata-backup-([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)-[0-9a-f]{6}\.jsonl$`)

// Settings owns one database's destination. Export must atomically publish a
// complete snapshot at the supplied filename and return only after it commits.
type Settings struct {
	Dir, StorageID   string
	Interval, Retain time.Duration
	Export           func(context.Context, string) error
}

// Clock permits interval and retention tests without waiting in real time.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// SystemClock is the daemon's wall clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// After waits for the requested wall-clock duration.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Schedule exports serially and prunes only after successful publication.
type Schedule struct {
	settings Settings
	dir      string
}

// New prepares an owner-only subdirectory keyed by the selected storage.
func New(settings Settings) (*Schedule, error) {
	if strings.TrimSpace(settings.Dir) == "" || !storageIDPattern.MatchString(settings.StorageID) || settings.Interval <= 0 || settings.Retain <= 0 || settings.Export == nil {
		return nil, fmt.Errorf("backup requires a directory, valid storage identity, positive durations, and an exporter")
	}
	dir := filepath.Join(settings.Dir, settings.StorageID)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	return &Schedule{settings: settings, dir: dir}, nil
}

// Run takes an immediate snapshot, then waits an interval after each attempt.
// Failed exports leave earlier backups intact and retry on the next interval.
func (s *Schedule) Run(ctx context.Context, clock Clock, report func(error)) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.snapshot(ctx, clock.Now()); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(s.settings.Interval):
		}
	}
}

func (s *Schedule) snapshot(ctx context.Context, now time.Time) error {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	name := "kata-backup-" + now.UTC().Format(timestampLayout) + "-" + hex.EncodeToString(suffix[:]) + ".jsonl"
	if err := s.settings.Export(ctx, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("backup export: %w", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("backup retention: %w", err)
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Name() == name || !entry.Type().IsRegular() {
			continue
		}
		match := ownedFilename.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		created, err := time.Parse(timestampLayout, match[1])
		if err != nil || !created.Before(now.Add(-s.settings.Retain)) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			return fmt.Errorf("backup retention: %w", err)
		}
	}
	return nil
}
