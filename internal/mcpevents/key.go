package mcpevents

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"go.kenn.io/kata/internal/config"
)

const keySize = 32

// ErrKeyMissing reports that no Events key exists yet. It is distinct from a
// corrupt key, which must never be replaced.
var ErrKeyMissing = errors.New("MCP Events key is missing")

// KeyProvider supplies the key that seals subscription secrets and signs
// cursors. Load never creates a key.
type KeyProvider interface {
	// Load returns the key, or ErrKeyMissing when none exists.
	Load(ctx context.Context) ([]byte, error)
	// Create returns the existing key, or publishes a new one if none exists.
	// Concurrent callers converge on one complete key.
	Create(ctx context.Context) ([]byte, error)
}

type fileKeyProvider struct{ path string }

// NewFileKeyProvider returns a provider backed by an owner-only 32-byte file.
func NewFileKeyProvider(path string) KeyProvider { return fileKeyProvider{path: path} }

// KeyCheck returns the value stored in meta.mcp_events_key_check: the hex
// HMAC-SHA256 of a fixed label under key. It identifies key material without
// revealing it.
func KeyCheck(key []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte("kata-mcp-events-key-check"))
	return hex.EncodeToString(m.Sum(nil))
}

func (p fileKeyProvider) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The open does not block on a FIFO and requires the effective uid to own
	// the file; type, size and mode are checked below.
	file, err := config.OpenCredentialFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrKeyMissing
		}
		return nil, fmt.Errorf("open MCP Events key: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat MCP Events key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != keySize || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("MCP Events key must be a 32-byte owner-only regular file")
	}
	key, err := io.ReadAll(io.LimitReader(file, keySize+1))
	if err != nil || len(key) != keySize {
		return nil, errors.New("cannot read MCP Events key")
	}
	return key, nil
}

func (p fileKeyProvider) Create(ctx context.Context) ([]byte, error) {
	key, err := p.Load(ctx)
	if err == nil || !errors.Is(err, ErrKeyMissing) {
		return key, err
	}
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create MCP Events key directory: %w", err)
	}
	fresh := make([]byte, keySize)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generate MCP Events key: %w", err)
	}
	// CreateTemp makes a 0600 file in the destination directory.
	file, err := os.CreateTemp(dir, ".mcp-events-key-*")
	if err != nil {
		return nil, fmt.Errorf("create MCP Events key: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(fresh); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write MCP Events key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync MCP Events key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close MCP Events key: %w", err)
	}
	// Publish a completely written file without replacing another creator's key.
	if err := os.Link(file.Name(), p.path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("publish MCP Events key: %w", err)
	}
	return p.Load(ctx)
}
