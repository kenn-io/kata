package hooks

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// runsAppender owns one *os.File and a mutex. Append marshals one
// runRecord, writes it as a single line, and rotates if the file size
// crosses the threshold.
type runsAppender struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	file    *os.File
	size    int64
}

func newRunsAppender(path string, maxSize int64, keep int) (*runsAppender, error) {
	if maxSize <= 0 || keep < 1 {
		return nil, fmt.Errorf("runs appender: maxSize=%d keep=%d invalid", maxSize, keep)
	}
	a := &runsAppender{path: path, maxSize: maxSize, keep: keep}
	f, err := a.openActive()
	if err != nil {
		return nil, fmt.Errorf("open runs.jsonl: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat runs.jsonl: %w", err)
	}
	a.file = f
	a.size = st.Size()
	return a, nil
}

// openActive opens a.path with the appender's standard flags. Centralized
// here so the four open sites (init, post-close recovery, rotate-final,
// reopenActive) share one //nolint:gosec justification.
func (a *runsAppender) openActive() (*os.File, error) {
	return os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: path is daemon-controlled state-dir filename
}

func (a *runsAppender) Append(r runRecord) {
	if r.Version == 0 {
		r.Version = 1
	}
	line, err := json.Marshal(r)
	if err != nil {
		// Marshal of our own struct should never fail; if it does,
		// drop the record rather than crash the worker.
		return
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		// Appender disabled until the next rotation attempt restores a
		// handle; drop the record rather than panic on nil deref.
		return
	}
	n, _ := a.file.Write(line)
	a.size += int64(n)
	if a.size >= a.maxSize {
		// Rotation failure is best-effort: subsequent appends keep
		// targeting the active file (or are dropped via the nil-guard
		// if reopen also failed). Append uptime > size cap.
		_ = a.rotateLocked()
	}
}

func (a *runsAppender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}

// rotateLocked closes the active file, renames runs.jsonl.K → .K+1
// (dropping the file beyond keep), renames the active file to .1, and
// opens a fresh active file. Caller holds a.mu.
//
// Recovery: every error path tries to leave a.file pointing at a
// usable handle so subsequent Append calls don't panic on nil. If the
// rotation step fails after we've already closed the active file, we
// reopen the original active path in append mode before returning the
// error. Callers log and continue with the (possibly oversize) active
// file — append uptime is the priority over hitting the size cap.
func (a *runsAppender) rotateLocked() error {
	if err := a.file.Close(); err != nil {
		// Failed to close: try to reopen so subsequent appends still
		// hit a valid handle.
		if f, openErr := a.openActive(); openErr == nil {
			a.file = f
		}
		return fmt.Errorf("close active before rotate: %w", err)
	}
	a.file = nil
	if err := a.shiftRotated(); err != nil {
		a.reopenActive()
		return err
	}
	if err := os.Rename(a.path, a.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.reopenActive()
		return fmt.Errorf("rename active -> .1: %w", err)
	}
	f, err := a.openActive()
	if err != nil {
		// Active was renamed to .1; reopening creates a fresh file.
		// If even that fails, append is broken until the next rotate
		// attempt — there is no safer recovery available.
		return fmt.Errorf("open fresh active: %w", err)
	}
	a.file = f
	a.size = 0
	return nil
}

// shiftRotated drops the oldest rotated file then shifts each
// remaining `.N` to `.N+1`. Caller holds a.mu and has already closed
// the active file.
func (a *runsAppender) shiftRotated() error {
	for i := a.keep; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", a.path, i)
		if i == a.keep {
			if err := os.Remove(from); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", from, err)
			}
			continue
		}
		to := fmt.Sprintf("%s.%d", a.path, i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rename %s -> %s: %w", from, to, err)
		}
	}
	return nil
}

// reopenActive ensures a.file is non-nil after a partial rotation by
// reopening the active path in append mode. If the open itself fails,
// a.file stays nil and Append's nil-check skips the write rather than
// panicking; the next rotateLocked attempt may succeed.
func (a *runsAppender) reopenActive() {
	if f, err := a.openActive(); err == nil {
		a.file = f
		if st, statErr := os.Stat(a.path); statErr == nil {
			a.size = st.Size()
		}
	}
}
