package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/atomicfile"
)

var cronSeenPathForTUI = func() (string, error) {
	home, err := config.KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "tui", "cron-seen.json"), nil
}

type cronSeenStore struct {
	Version  int                        `json:"version"`
	Projects map[string]cronSeenProject `json:"projects"`
}
type cronSeenProject struct {
	ClearedThrough time.Time `json:"cleared_through"`
	Runs           []string  `json:"runs"`
}

// Capped union is deterministic regardless of writer order. Eviction can make
// an old UID unseen again; the watermark is the persistent clear-all boundary.
func cronSeenMerge(a, b cronSeenStore) cronSeenStore {
	out := cronSeenStore{Version: 1, Projects: map[string]cronSeenProject{}}
	for _, store := range []cronSeenStore{a, b} {
		for project, p := range store.Projects {
			old := out.Projects[project]
			if p.ClearedThrough.After(old.ClearedThrough) {
				old.ClearedThrough = p.ClearedThrough.UTC()
			}
			ids := append(slices.Clone(old.Runs), p.Runs...)
			slices.Sort(ids)
			ids = slices.Compact(ids)
			if len(ids) > 0 && ids[0] == "" {
				ids = ids[1:]
			}
			if len(ids) > 1000 {
				ids = ids[len(ids)-1000:]
			}
			if len(ids) == 0 {
				ids = nil
			}
			old.Runs = slices.Clone(ids)
			out.Projects[project] = old
		}
	}
	return out
}
func cronSeenDecode(raw []byte) (cronSeenStore, error) {
	var s cronSeenStore
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Version != 1 {
		return s, errors.New("unsupported cron seen state version")
	}
	return cronSeenMerge(s, cronSeenStore{}), nil
}
func cronSeenLoad(ctx context.Context, path string) (cronSeenStore, error) {
	if err := ctx.Err(); err != nil {
		return cronSeenStore{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cronSeenMerge(cronSeenStore{}, cronSeenStore{}), nil
	}
	if err != nil {
		return cronSeenStore{}, err
	}
	return cronSeenDecode(raw)
}
func cronSeenSave(ctx context.Context, path string, data cronSeenStore) (cronSeenStore, error) {
	if err := ctx.Err(); err != nil {
		return cronSeenStore{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return cronSeenStore{}, err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return cronSeenStore{}, err
	}
	if !locked {
		return cronSeenStore{}, errors.New("cron seen state lock unavailable")
	}
	// Unlock must complete after cancellation so another TUI can persist its state.
	defer func() { _ = lock.Unlock() }()
	disk, err := cronSeenLoad(ctx, path)
	if err != nil {
		return cronSeenStore{}, err
	}
	merged := cronSeenMerge(disk, data)
	raw, err := json.Marshal(merged)
	if err != nil {
		return cronSeenStore{}, err
	}
	if err := ctx.Err(); err != nil {
		return cronSeenStore{}, err
	}
	if err := atomicfile.WriteFile(path, raw, atomicfile.WithPrivate()); err != nil {
		return cronSeenStore{}, err
	}
	return merged, nil
}
func cronFailureTime(r generated.CronRun) time.Time {
	if r.EndedAt != nil {
		return *r.EndedAt
	}
	return r.UpdatedAt
}
func (p cronSeenProject) unseen(r generated.CronRun) bool {
	return r.UID != "" && (r.Status == "failed" || r.Status == "unknown") && cronFailureTime(r).After(p.ClearedThrough) && !slices.Contains(p.Runs, r.UID)
}
