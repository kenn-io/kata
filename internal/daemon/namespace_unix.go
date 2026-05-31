//go:build !windows

package daemon

import (
	"os"
	"strconv"
)

// userTag returns the per-user component of the socket directory name.
// On Unix the numeric uid is canonical and matches every existing kata
// runtime that's already been deployed.
func userTag() string {
	return strconv.Itoa(os.Getuid())
}
