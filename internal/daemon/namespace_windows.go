//go:build windows

package daemon

import (
	"os"
	"strings"
)

// userTag returns the per-user component of the socket directory name.
// Windows does not have a meaningful uid (os.Getuid returns -1), so we
// derive a label from the USERNAME environment variable. Characters
// that are not safe in a path segment are replaced with '_'.
func userTag() string {
	name := os.Getenv("USERNAME")
	if name == "" {
		name = "user"
	}
	return sanitizePathSegment(name)
}

// sanitizePathSegment replaces characters that are illegal in a Windows
// path component (or that would surprise a path-walker) with '_'. The
// allowlist is intentionally narrow so unusual usernames cannot smuggle
// in separators or shell metacharacters.
func sanitizePathSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "user"
	}
	return b.String()
}
