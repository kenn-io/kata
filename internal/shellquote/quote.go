// Package shellquote renders arguments for POSIX shells.
package shellquote

import "strings"

// Quote returns s as one POSIX shell argument, quoting it only when it
// holds characters outside the plain command-line set.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./:=,", r)
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
