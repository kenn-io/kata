//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func replaceFederationCredentialsFileOnDisk(source, target string) error {
	return os.Rename(source, target)
}

func syncFederationCredentialsDirectory(dir string) (retErr error) {
	directory, err := os.Open(dir) //nolint:gosec // dir is derived from KATA_HOME.
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, directory.Close())
	}()
	if err := directory.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
