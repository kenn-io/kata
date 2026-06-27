package hooks

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// knownEventTypes is the persisted-events set per master spec §3.3. New
// entries belong here whenever a new hook-eligible event is added.
var knownEventTypes = map[string]struct{}{
	"issue.created": {}, "issue.updated": {}, "issue.closed": {},
	"issue.reopened": {}, "issue.commented": {}, "issue.comment_edited": {}, "issue.linked": {},
	"issue.unlinked": {}, "issue.labeled": {}, "issue.unlabeled": {},
	"issue.assigned": {}, "issue.unassigned": {},
	"issue.priority_set": {}, "issue.priority_cleared": {},
	"issue.soft_deleted": {}, "issue.restored": {},
	"issue.links_changed": {},
	// close.throttled lives outside the issue.* namespace because the
	// audit signal is about a refused mutation, not the issue itself.
	// Wildcard `*` hooks see it; `issue.*` does not.
	"close.throttled": {},
}

// compileEventMatcher returns the canonical string and a precompiled
// matcher for the three accepted forms; an error otherwise. Both `*`
// and `issue.*` only match types in knownEventTypes, so an unknown
// (e.g., future-typo) "issue.foo" event won't quietly fan out to all
// issue.* hooks.
func compileEventMatcher(raw string) (string, func(string) bool, error) {
	switch raw {
	case "":
		return "", nil, fmt.Errorf("event must be set")
	case "*":
		return "*", func(t string) bool { _, ok := knownEventTypes[t]; return ok }, nil
	case "issue.*":
		return "issue.*", func(t string) bool {
			if !strings.HasPrefix(t, "issue.") {
				return false
			}
			_, ok := knownEventTypes[t]
			return ok
		}, nil
	case "sync.reset_required":
		return "", nil, fmt.Errorf("event %q is synthetic and cannot be hooked", raw)
	}
	if _, ok := knownEventTypes[raw]; ok {
		want := raw
		return raw, func(t string) bool { return t == want }, nil
	}
	return "", nil, fmt.Errorf("event %q is not a known event type or pattern", raw)
}

// validateCommand: absolute path OR bare name (no path separators).
// Rejects ./foo, bin/foo, "", ".", "..", leading/trailing whitespace,
// NUL bytes, and control characters. Internal whitespace is allowed in
// absolute paths (e.g. /Applications/Some App/bin/x) but rejected in
// bare names since exec.Command treats bare names as PATH lookups —
// "my command" would try to find an executable literally named
// "my command".
func validateCommand(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("command must be non-empty")
	}
	if strings.TrimSpace(cmd) != cmd {
		return fmt.Errorf("command %q must not contain leading/trailing whitespace", cmd)
	}
	if strings.ContainsAny(cmd, "\t\n\r\x00") {
		return fmt.Errorf("command %q must not contain control characters or NUL", cmd)
	}
	if cmd == "." || cmd == ".." {
		return fmt.Errorf("command %q is a directory reference, not an executable", cmd)
	}
	if filepath.IsAbs(cmd) {
		return nil
	}
	if strings.ContainsRune(cmd, ' ') {
		return fmt.Errorf("command %q must be absolute to contain spaces (bare names are PATH-looked-up by exec.Command)", cmd)
	}
	if strings.ContainsRune(cmd, '/') || (filepath.Separator != '/' && strings.ContainsRune(cmd, filepath.Separator)) {
		return fmt.Errorf("command %q must be absolute or a bare name (no path separators)", cmd)
	}
	return nil
}

// validateWorkingDir: absolute after filepath.Clean. We only check
// shape; existence is checked at fire time per spec §6.1.
func validateWorkingDir(p string) error {
	if p == "" {
		return nil
	}
	cleaned := filepath.Clean(p)
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("working_dir %q must be absolute", p)
	}
	return nil
}

// validateTimeout: (0, 5m].
func validateTimeout(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("timeout must be > 0")
	}
	if d > 5*time.Minute {
		return fmt.Errorf("timeout must be <= 5m")
	}
	return nil
}

// validateUserEnv: keys must not match ^KATA_. Returns sorted KEY=VAL slice.
func validateUserEnv(m map[string]string) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, "KATA_") {
			return nil, fmt.Errorf("env key %q is reserved (^KATA_ keys are set by the dispatcher)", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

// parseSize accepts bare integer bytes or the suffixes k/kb/m/mb
// (binary, case-insensitive). Returns the byte count. Rejects values
// that would overflow int64 after multiplying by the unit.
func parseSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("size must be non-empty")
	}
	mul := int64(1)
	switch {
	case strings.HasSuffix(t, "mb"):
		mul, t = 1024*1024, strings.TrimSuffix(t, "mb")
	case strings.HasSuffix(t, "kb"):
		mul, t = 1024, strings.TrimSuffix(t, "kb")
	case strings.HasSuffix(t, "m"):
		mul, t = 1024*1024, strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "k"):
		mul, t = 1024, strings.TrimSuffix(t, "k")
	}
	t = strings.TrimSpace(t)
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size %q must be > 0", s)
	}
	if n > math.MaxInt64/mul {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mul, nil
}

// parseDuration is a small wrapper that produces a clearer error message
// than time.ParseDuration alone.
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}
