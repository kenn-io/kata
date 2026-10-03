//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !solaris

package hooks

import "os"

func workingDirectoryAvailable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
