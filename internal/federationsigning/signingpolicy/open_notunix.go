//go:build !unix

package signingpolicy

import "os"

func openKeyFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: operator-configured secret source.
}
