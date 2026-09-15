// Package tokenfile writes one-time credentials without exposing them through
// command output or replacing an existing filesystem entry.
package tokenfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Reservation is an exclusively created token file that has not yet received
// a secret. Call Abort on every path that does not successfully call Commit.
type Reservation struct {
	path      string
	file      *os.File
	committed bool
}

// Reserve creates path exclusively in a private directory. Existing files,
// symlinks, missing directories, and non-private directories are rejected.
func Reserve(path string) (*Reservation, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("token file path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve token file path: %w", err)
	}
	parent := filepath.Dir(abs)
	physicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve token file directory %s: %w", parent, err)
	}
	abs = filepath.Join(physicalParent, filepath.Base(abs))
	info, err := os.Stat(physicalParent)
	if err != nil {
		return nil, fmt.Errorf("inspect token file directory %s: %w", physicalParent, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("token file directory %s is not a directory", physicalParent)
	}
	if err := validatePrivateDirectory(physicalParent, info); err != nil {
		return nil, err
	}
	file, err := openExclusive(abs)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("token file %s already exists", abs)
		}
		return nil, fmt.Errorf("reserve token file %s: %w", abs, err)
	}
	return &Reservation{path: abs, file: file}, nil
}

// Path returns the absolute path reserved for the token.
func (r *Reservation) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Commit writes the credential, flushes it, and closes the reservation. A
// partial or failed delivery removes the file.
func (r *Reservation) Commit(secret string) (retErr error) {
	if r == nil || r.file == nil {
		return errors.New("token file reservation is not open")
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("token plaintext is empty")
	}
	defer func() {
		if r.committed {
			return
		}
		retErr = errors.Join(retErr, r.abort())
	}()
	if _, err := io.WriteString(r.file, secret+"\n"); err != nil {
		return fmt.Errorf("write token file %s: %w", r.path, err)
	}
	if err := r.file.Sync(); err != nil {
		return fmt.Errorf("sync token file %s: %w", r.path, err)
	}
	if err := r.file.Close(); err != nil {
		r.file = nil
		return fmt.Errorf("close token file %s: %w", r.path, err)
	}
	r.file = nil
	r.committed = true
	return nil
}

// Abort closes and removes an unused reservation. It is safe after Commit.
func (r *Reservation) Abort() error {
	if r == nil || r.committed {
		return nil
	}
	return r.abort()
}

func (r *Reservation) abort() error {
	var result error
	if r.file != nil {
		result = errors.Join(result, r.file.Close())
		r.file = nil
	}
	if r.path != "" {
		if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove token file reservation %s: %w", r.path, err))
		}
	}
	return result
}
