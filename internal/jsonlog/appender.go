// Package jsonlog writes best-effort local diagnostics with bounded rotation.
package jsonlog

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/atomicfile"
)

// Appender serializes whole records. Failed opens are retried on the next
// Append; its caller decides how to report a lost diagnostic record.
type Appender struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	file    *os.File
	size    int64
	closed  bool
}

// New constructs a lazy appender. Nonpositive limits use hook-log defaults.
func New(path string, maxSize int64, keep int) *Appender {
	if maxSize <= 0 {
		maxSize = 50 * 1024 * 1024
	}
	if keep <= 0 {
		keep = 5
	}
	return &Appender{path: path, maxSize: maxSize, keep: keep}
}

// Append writes one JSON value and newline, then rotates at the size limit.
func (a *Appender) Append(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return os.ErrClosed
	}
	if a.file == nil {
		if err := a.open(ctx); err != nil {
			return err
		}
	}
	n, err := a.file.Write(line)
	a.size += int64(n)
	if err != nil || n != len(line) {
		_ = a.file.Close()
		a.file = nil
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if a.size >= a.maxSize {
		return a.rotate(ctx)
	}
	return nil
}

func (a *Appender) open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // daemon-controlled local diagnostics path
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	// A failed write or previous process can leave an incomplete record. Fence
	// that fragment with a newline so the next successful append is readable.
	size := info.Size()
	if size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			_ = f.Close()
			return err
		}
		if last[0] != '\n' {
			n, err := f.Write([]byte{'\n'})
			if err == nil && n != 1 {
				err = io.ErrShortWrite
			}
			if err != nil {
				_ = f.Close()
				return err
			}
			size++
		}
	}
	a.file, a.size = f, size
	return nil
}

func (a *Appender) rotate(ctx context.Context) error {
	err := a.file.Close()
	a.file = nil
	if err != nil {
		return err
	}
	for i := a.keep; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", a.path, i)
		if i == a.keep {
			err = os.Remove(from)
		} else {
			err = atomicfile.Replace(from, fmt.Sprintf("%s.%d", a.path, i+1))
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := atomicfile.Replace(a.path, a.path+".1"); err != nil {
		return err
	}
	return a.open(ctx)
}

// Close releases the handle after the owning worker has stopped.
func (a *Appender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}
