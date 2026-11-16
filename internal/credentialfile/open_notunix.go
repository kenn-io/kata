//go:build !unix

// Package credentialfile opens operator-configured credential files.
package credentialfile

import "os"

// Open opens a credential file.
func Open(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: path comes from the operator's key-file configuration.
}
