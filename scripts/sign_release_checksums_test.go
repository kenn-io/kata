package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/selfupdate"
)

func TestSignChecksumsSignsKitPayload(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dir := t.TempDir()
	checksumsPath := filepath.Join(dir, "SHA256SUMS")
	const checksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const asset = "kata_0.5.0_linux_amd64.tar.gz"
	require.NoError(t, os.WriteFile(checksumsPath, []byte(checksum+"  "+asset+"\n"), 0o600))

	require.NoError(t, signChecksums(checksumsPath, "kenn-io", "kata", "v0.5.0", privateKey))

	//nolint:gosec // test reads the signature path produced under t.TempDir.
	sigHex, err := os.ReadFile(filepath.Join(dir, asset+".sha256.sig"))
	require.NoError(t, err)
	signature, err := hex.DecodeString(string(sigHex[:len(sigHex)-1]))
	require.NoError(t, err)
	payload := selfupdate.SignaturePayload(selfupdate.SignatureMetadata{
		Owner:    "kenn-io",
		Repo:     "kata",
		Version:  "v0.5.0",
		Asset:    asset,
		GOOS:     "linux",
		GOARCH:   "amd64",
		Checksum: checksum,
	})
	require.True(t, ed25519.Verify(publicKey, payload, signature))
}
