//go:build windows

package config

import "golang.org/x/sys/windows"

func replaceFederationCredentialsFileOnDisk(source, target string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePath,
		targetPath,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncFederationCredentialsDirectory(string) error {
	// MOVEFILE_WRITE_THROUGH waits for the replacement to reach disk. Windows
	// does not expose the Unix parent-directory fsync operation.
	return nil
}
