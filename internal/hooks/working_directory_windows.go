//go:build windows

package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func workingDirectoryAvailable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}

	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return false
	}
	probePath, err := windows.UTF16PtrFromString(filepath.Join(path, ".kata-doctor-access-"+hex.EncodeToString(suffix[:])))
	if err != nil {
		return false
	}
	handle, err := windows.CreateFile(
		probePath,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		_ = windows.CloseHandle(handle)
		return true
	}
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
