// Package hooks implements the daemon's post-commit hook dispatcher. See the
// hooks section of docs/design/architecture.md for the design rationale (the
// after-commit, no-shell, not-a-sandbox delivery contract).
package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// ResolvedHook is one [[hook]] entry after parsing and validation. The
// Match func is precompiled at load so the dispatcher's hot path does no
// string comparisons beyond what Match does internally.
type ResolvedHook struct {
	Event      string            // canonical literal from TOML: "issue.created" | "issue.*" | "*"
	Match      func(string) bool // precompiled; tested against evt.Type
	Command    string            // absolute path or bare name (no '/')
	Args       []string
	Timeout    time.Duration
	WorkingDir string   // absolute, defaulted to $KATA_HOME at load
	UserEnv    []string // ["KEY=VAL", ...]; sorted by key for determinism
	Index      int      // 0-based source-file ordinal (used in output filenames)
}

// Snapshot is the live-reloadable hook set. Stored behind atomic.Pointer
// in the dispatcher; replaced wholesale on Reload.
type Snapshot struct {
	Hooks []ResolvedHook
}

// Config holds the [hooks] tunables. Startup-only in v1 — Reload may
// observe diffs but never applies them; see LoadedConfig.UnchangedTunables.
type Config struct {
	PoolSize             int
	QueueCap             int
	OutputDiskCap        int64
	RunsLogMaxBytes      int64
	RunsLogKeep          int
	QueueFullLogInterval time.Duration
}

// LoadedConfig bundles a parse result. UnchangedTunables is populated by
// LoadReload only — it lists human-readable strings like
// "pool_size: requested 8, active 4 (restart required)" so the daemon
// can log them without recomputing diffs itself.
type LoadedConfig struct {
	Snapshot          Snapshot
	Config            Config
	UnchangedTunables []string
}

// HookJob is the unit pushed onto the dispatcher queue. The hook is
// captured by value at enqueue time so the worker never re-reads the
// snapshot pointer; this is what makes Reload safe with in-flight jobs.
type HookJob struct {
	Event      db.Event
	Hook       ResolvedHook
	EnqueuedAt time.Time
}

// IssueSnapshot is the resolver output that fills the issue block of
// the stdin payload. Read at fire time, not enqueue time.
//
// ShortID replaces the legacy per-project Number sequence (spec §9.7);
// it is a display snapshot that may shift across short_id cutovers or
// federation merges. UID is the canonical, stable issue identity —
// hook subscribers that key on identity should consume UID, while the
// short_id remains available for rendering "issue {short_id}: {title}".
type IssueSnapshot struct {
	UID     string
	ShortID string
	Title   string
	Status  string
	Labels  []string // sorted for determinism
	Owner   string
	Author  string
}

// CommentSnapshot fills payload.comment_body for issue.commented events.
type CommentSnapshot struct {
	ID   int64
	Body string
}

// ProjectSnapshot fills project.name. project.id and project.identity
// come from the event itself, so a project resolver failure only drops
// the human-readable name.
type ProjectSnapshot struct {
	Name string
}

// AliasSnapshot fills the alias block when the event has a single
// well-defined workspace.
type AliasSnapshot struct {
	Identity string // marshalled as alias_identity
	Kind     string // marshalled as alias_kind ("git" | "local")
	RootPath string // marshalled as root_path
}

// rawHookFile mirrors the on-disk schema. Strict TOML unmarshalling
// rejects unknown keys.
type rawHookFile struct {
	Hooks rawHooksSection `toml:"hooks"`
	Hook  []rawHookEntry  `toml:"hook"`
}

type rawHooksSection struct {
	PoolSize             *int       `toml:"pool_size"`
	QueueCap             *int       `toml:"queue_cap"`
	OutputDiskCap        *sizeValue `toml:"output_disk_cap"`
	RunsLogMax           *sizeValue `toml:"runs_log_max"`
	RunsLogKeep          *int       `toml:"runs_log_keep"`
	QueueFullLogInterval *string    `toml:"queue_full_log_interval"`
}

// sizeValue wraps a parsed byte count. UnmarshalTOML accepts both bare
// integer bytes (output_disk_cap = 100) and unit-suffixed strings
// (output_disk_cap = "100MB"), per spec §4.2.
type sizeValue struct{ Bytes int64 }

// UnmarshalTOML decodes either a TOML integer or a TOML string into a
// byte count. Unit suffixes are parsed by parseSize; bare integers go
// through the same > 0 / overflow checks as parsed strings.
func (s *sizeValue) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case int64:
		if t <= 0 {
			return fmt.Errorf("size %d must be > 0", t)
		}
		s.Bytes = t
		return nil
	case string:
		n, err := parseSize(t)
		if err != nil {
			return err
		}
		s.Bytes = n
		return nil
	default:
		return fmt.Errorf("size must be string or integer, got %T", v)
	}
}

type rawHookEntry struct {
	Event      string            `toml:"event"`
	Command    string            `toml:"command"`
	Args       []string          `toml:"args"`
	Timeout    string            `toml:"timeout"`
	WorkingDir string            `toml:"working_dir"`
	Env        map[string]string `toml:"env"`
}

const maxHookEntries = 256

// defaultConfig returns the v1 baseline tunables.
func defaultConfig() Config {
	return Config{
		PoolSize:             4,
		QueueCap:             1000,
		OutputDiskCap:        100 * 1024 * 1024,
		RunsLogMaxBytes:      50 * 1024 * 1024,
		RunsLogKeep:          5,
		QueueFullLogInterval: 60 * time.Second,
	}
}

// LoadStartup reads hooks.toml at daemon start. Missing file is not an
// error; malformed file is. Errors are fatal at the caller.
func LoadStartup(path string) (LoadedConfig, error) {
	return loadFile(path, defaultConfig(), nil)
}

// LoadReload reads hooks.toml in response to SIGHUP. Missing file is not
// an error and yields an empty Snapshot. Tunable diffs vs current are
// captured into LoadedConfig.UnchangedTunables; the Config itself comes
// straight from current (live-reload of [hooks] is YAGNI in v1).
func LoadReload(path string, current Config) (LoadedConfig, error) {
	return loadFile(path, current, &current)
}

func loadFile(path string, base Config, prevForDiff *Config) (LoadedConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-supplied hooks config location
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LoadedConfig{Config: base}, nil
		}
		return LoadedConfig{}, fmt.Errorf("read hooks config %s: %w", path, err)
	}
	var raw rawHookFile
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("parse hooks config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return LoadedConfig{}, fmt.Errorf("hooks config %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	parsed, err := buildLoadedConfig(raw, base)
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("hooks config %s: %w", path, err)
	}
	if prevForDiff != nil {
		parsed.UnchangedTunables = diffTunables(*prevForDiff, parsed.Config)
		parsed.Config = *prevForDiff
	}
	return parsed, nil
}

func buildLoadedConfig(raw rawHookFile, base Config) (LoadedConfig, error) {
	cfg := base
	if raw.Hooks.PoolSize != nil {
		v := *raw.Hooks.PoolSize
		if v < 1 || v > 16 {
			return LoadedConfig{}, fmt.Errorf("pool_size %d not in [1,16]", v)
		}
		cfg.PoolSize = v
	}
	if raw.Hooks.QueueCap != nil {
		v := *raw.Hooks.QueueCap
		if v < 1 || v > 10000 {
			return LoadedConfig{}, fmt.Errorf("queue_cap %d not in [1,10000]", v)
		}
		cfg.QueueCap = v
	}
	if raw.Hooks.OutputDiskCap != nil {
		cfg.OutputDiskCap = raw.Hooks.OutputDiskCap.Bytes
	}
	if raw.Hooks.RunsLogMax != nil {
		cfg.RunsLogMaxBytes = raw.Hooks.RunsLogMax.Bytes
	}
	if raw.Hooks.RunsLogKeep != nil {
		v := *raw.Hooks.RunsLogKeep
		if v < 1 || v > 100 {
			return LoadedConfig{}, fmt.Errorf("runs_log_keep %d not in [1,100]", v)
		}
		cfg.RunsLogKeep = v
	}
	if raw.Hooks.QueueFullLogInterval != nil {
		d, err := parseDuration(*raw.Hooks.QueueFullLogInterval)
		if err != nil {
			return LoadedConfig{}, fmt.Errorf("queue_full_log_interval: %w", err)
		}
		if d < time.Second {
			return LoadedConfig{}, fmt.Errorf("queue_full_log_interval must be >= 1s, got %s", d)
		}
		cfg.QueueFullLogInterval = d
	}

	if len(raw.Hook) > maxHookEntries {
		return LoadedConfig{}, fmt.Errorf("%d [[hook]] entries (max %d)", len(raw.Hook), maxHookEntries)
	}

	home, err := config.KataHome()
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("resolve KATA_HOME: %w", err)
	}
	hooks := make([]ResolvedHook, 0, len(raw.Hook))
	for i, h := range raw.Hook {
		resolved, err := resolveHookEntry(h, i, home)
		if err != nil {
			return LoadedConfig{}, err
		}
		hooks = append(hooks, resolved)
	}
	return LoadedConfig{Snapshot: Snapshot{Hooks: hooks}, Config: cfg}, nil
}

func resolveHookEntry(h rawHookEntry, i int, home string) (ResolvedHook, error) {
	canon, match, err := compileEventMatcher(h.Event)
	if err != nil {
		return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
	}
	if err := validateCommand(h.Command); err != nil {
		return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
	}
	timeout := 30 * time.Second
	if h.Timeout != "" {
		d, err := parseDuration(h.Timeout)
		if err != nil {
			return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
		}
		if err := validateTimeout(d); err != nil {
			return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
		}
		timeout = d
	}
	wd := h.WorkingDir
	if err := validateWorkingDir(wd); err != nil {
		return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
	}
	if wd == "" {
		wd = home
	} else {
		wd = filepath.Clean(wd)
	}
	userEnv, err := validateUserEnv(h.Env)
	if err != nil {
		return ResolvedHook{}, fmt.Errorf("hook[%d]: %w", i, err)
	}
	args := h.Args
	if args == nil {
		args = []string{}
	}
	return ResolvedHook{
		Event:      canon,
		Match:      match,
		Command:    h.Command,
		Args:       args,
		Timeout:    timeout,
		WorkingDir: wd,
		UserEnv:    userEnv,
		Index:      i,
	}, nil
}

func diffTunables(prev, parsed Config) []string {
	var out []string
	if parsed.PoolSize != prev.PoolSize {
		out = append(out, fmt.Sprintf("pool_size: requested %d, active %d (restart required)", parsed.PoolSize, prev.PoolSize))
	}
	if parsed.QueueCap != prev.QueueCap {
		out = append(out, fmt.Sprintf("queue_cap: requested %d, active %d (restart required)", parsed.QueueCap, prev.QueueCap))
	}
	if parsed.OutputDiskCap != prev.OutputDiskCap {
		out = append(out, fmt.Sprintf("output_disk_cap: requested %d, active %d (restart required)", parsed.OutputDiskCap, prev.OutputDiskCap))
	}
	if parsed.RunsLogMaxBytes != prev.RunsLogMaxBytes {
		out = append(out, fmt.Sprintf("runs_log_max: requested %d, active %d (restart required)", parsed.RunsLogMaxBytes, prev.RunsLogMaxBytes))
	}
	if parsed.RunsLogKeep != prev.RunsLogKeep {
		out = append(out, fmt.Sprintf("runs_log_keep: requested %d, active %d (restart required)", parsed.RunsLogKeep, prev.RunsLogKeep))
	}
	if parsed.QueueFullLogInterval != prev.QueueFullLogInterval {
		out = append(out, fmt.Sprintf("queue_full_log_interval: requested %s, active %s (restart required)", parsed.QueueFullLogInterval, prev.QueueFullLogInterval))
	}
	return out
}
