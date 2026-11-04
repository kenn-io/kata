// Package jsonlog writes size-limited rotating JSONL streams.
package jsonlog

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/atomicfile"
)

// Appender writes complete JSONL records and retries failed opens on append.
type Appender struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	file    *os.File
	size    int64
}

// Open creates the directory and opens a cron decision stream.
func Open(ctx context.Context, path string, maxSize int64, keep int) (*Appender, error) {
	if maxSize <= 0 || keep < 1 {
		return nil, fmt.Errorf("jsonlog appender: maxSize=%d keep=%d invalid", maxSize, keep)
	}
	a := &Appender{path: path, maxSize: maxSize, keep: keep}
	if err := a.openActive(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Appender) openActive(_ context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // daemon-controlled log path
	if err != nil {
		return err
	}
	size, err := terminateTail(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	a.file, a.size = file, size
	return nil
}

// terminateTail ends an interrupted final record so the next append starts a
// new line. Readers skip the interrupted line if it is malformed.
func terminateTail(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return 0, err
	}
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return info.Size(), nil
	}
	n, err := file.Write([]byte{'\n'})
	return info.Size() + int64(n), err
}

// Append reports I/O failures without changing committed dispatch accounting.
func (a *Appender) Append(ctx context.Context, value any) error {
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		if err := a.openActive(ctx); err != nil {
			return err
		}
	}
	n, err := a.file.Write(line)
	a.size += int64(n)
	if err != nil {
		return err
	}
	if a.size >= a.maxSize {
		return a.rotateLocked(ctx)
	}
	return nil
}

// rotateLocked leaves a nil handle on failure so the next append reopens it.
func (a *Appender) rotateLocked(ctx context.Context) error {
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
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := atomicfile.Replace(a.path, a.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return a.openActive(ctx)
}

// Close releases the stream after its sweeper has stopped.
func (a *Appender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}
