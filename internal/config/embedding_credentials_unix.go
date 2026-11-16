//go:build unix

package config

import (
	"os"

	"golang.org/x/sys/unix"

	"go.kenn.io/kata/internal/credentialfile"
)

func openEmbeddingCredentialFile(path string) (*os.File, error) {
	f, err := credentialfile.Open(path)
	if err != nil {
		return nil, err
	}
	var fileInfo unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &fileInfo); err != nil {
		_ = f.Close()
		return nil, err
	}
	if int64(fileInfo.Uid) != int64(os.Geteuid()) {
		_ = f.Close()
		return nil, errEmbeddingCredentialWrongOwner
	}
	return f, nil
}
