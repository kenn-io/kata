package config

import "os"

// OpenCredentialFile opens a secret file the way credential readers must: it
// never blocks on a FIFO and, where the platform allows, requires the file to
// belong to the effective user. Callers still check that the result is a
// regular file with the size and mode they require.
func OpenCredentialFile(path string) (*os.File, error) {
	return openEmbeddingCredentialFile(path)
}
