// Package editorcmd resolves the user's interactive editor command.
package editorcmd

import (
	"os"
	"strings"
)

// Resolve honors VISUAL before EDITOR, splits arguments on whitespace, and
// falls back to vi. The command is intentionally not evaluated by a shell.
func Resolve() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if parts := strings.Fields(os.Getenv(env)); len(parts) > 0 {
			return parts
		}
	}
	return []string{"vi"}
}
