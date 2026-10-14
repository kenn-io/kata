package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ReadRootSigningKey reads the owner-only local root signing key. A missing
// file returns an error matching os.ErrNotExist and is never created here.
// Keys are not part of ordinary backups or transport records.
func ReadRootSigningKey(path string) (ed25519.PrivateKey, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("root signing key requires an absolute path")
	}
	return readRootSigningKey(path)
}

// LoadOrCreateRootSigningKey reads the local root signing key, creating a new
// one only when the file is missing. An existing file is never replaced.
func LoadOrCreateRootSigningKey(path string) (ed25519.PrivateKey, error) {
	key, err := ReadRootSigningKey(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".root-key-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := io.WriteString(file, base64.StdEncoding.EncodeToString(private.Seed())+"\n"); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	// Publish a completely written file without replacing another creator's key.
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return readRootSigningKey(path)
}

func readRootSigningKey(path string) (ed25519.PrivateKey, error) {
	file, err := openEmbeddingCredentialFile(path) //nolint:gosec // Owner-configured key path; descriptor ownership, type, size and mode are checked.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("root signing key must be a small owner-only regular file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil || len(encoded) > 256 {
		return nil, errors.New("cannot read root signing key")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid root signing key encoding")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
