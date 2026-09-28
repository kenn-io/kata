//go:build !unix

package config

import "os"

func openEmbeddingCredentialFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: path comes from the operator's key-file configuration.
}
