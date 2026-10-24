// Package editorcmd resolves the user-configured terminal editor.
package editorcmd

import (
	"os"
	"strings"
)

// Resolve honors VISUAL before EDITOR and falls back to vi.
func Resolve() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			parts := strings.Fields(v)
			if len(parts) > 0 {
				return parts
			}
		}
	}
	return []string{"vi"}
}
