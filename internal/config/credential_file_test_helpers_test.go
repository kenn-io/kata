package config

import (
	"testing"

	"go.kenn.io/kit/safefileio"
)

func writePrivateCredentialFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
