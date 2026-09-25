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
