//go:build !unix

package config

import (
	"os"

	"go.kenn.io/kata/internal/credentialfile"
)

func openEmbeddingCredentialFile(path string) (*os.File, error) {
	return credentialfile.Open(path)
}
