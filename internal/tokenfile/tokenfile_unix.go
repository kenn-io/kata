//go:build !windows

package tokenfile

import (
	"fmt"
	"os"
)

func validatePrivateDirectory(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("token file directory %s must be owner-only", path)
	}
	return nil
}

func openExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // caller-selected path is exclusively created in an owner-only directory.
}
