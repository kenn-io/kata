package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Hydration can read after a mutation commits or after collection selection.
// Recheck every exposed issue after building the response so an intervening
// move cannot disclose content added outside the authorized subtree.
func validateScopedResponse(ctx context.Context, store db.Storage, scope db.APITokenScope) error {
	if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
		return err
	}
	targets := db.IssueScopeTargets(ctx)
	if len(targets) == 0 {
		return nil
	}
	allowed, err := store.IssueInScope(ctx, scope, targets...)
	if err != nil {
		return err
	}
	if !allowed {
		return api.NewError(http.StatusNotFound, "issue_not_found", "issue not found", "", nil)
	}
	return nil
}

type bufferedScopedResponse struct {
	underlying http.ResponseWriter
	header     http.Header
	body       bytes.Buffer
	status     int
	spill      *os.File
	writeErr   error
}

const scopedResponseMemoryLimit = 1 << 20

func newBufferedScopedResponse(w http.ResponseWriter) *bufferedScopedResponse {
	return &bufferedScopedResponse{underlying: w, header: make(http.Header)}
}

func (w *bufferedScopedResponse) Header() http.Header         { return w.header }
func (w *bufferedScopedResponse) Unwrap() http.ResponseWriter { return w.underlying }

func (w *bufferedScopedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedScopedResponse) Write(body []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.spill == nil && w.body.Len()+len(body) > scopedResponseMemoryLimit {
		if err := w.startSpill(); err != nil {
			w.writeErr = err
			return 0, err
		}
	}
	if w.spill != nil {
		n, err := w.spill.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.writeErr = err
		}
		return n, err
	}
	return w.body.Write(body)
}

func (w *bufferedScopedResponse) startSpill() error {
	file, err := os.CreateTemp("", "kata-scoped-response-")
	if err != nil {
		return err
	}
	if w.body.Len() > 0 {
		n, writeErr := file.Write(w.body.Bytes())
		if writeErr == nil && n != w.body.Len() {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			closeErr := file.Close()
			removeErr := os.Remove(file.Name())
			return errors.Join(writeErr, closeErr, removeErr)
		}
	}
	w.body = bytes.Buffer{}
	w.spill = file
	return nil
}

func (w *bufferedScopedResponse) prepare() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.spill != nil {
		if _, err := w.spill.Seek(0, io.SeekStart); err != nil {
			w.writeErr = err
			return err
		}
	}
	return nil
}

func (w *bufferedScopedResponse) writeTo(destination http.ResponseWriter) {
	for key, values := range w.header {
		destination.Header()[key] = append([]string(nil), values...)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	destination.WriteHeader(status)
	if w.spill != nil {
		_, _ = io.Copy(destination, w.spill)
		return
	}
	_, _ = destination.Write(w.body.Bytes())
}

func (w *bufferedScopedResponse) close() error {
	if w.spill == nil {
		return nil
	}
	name := w.spill.Name()
	closeErr := w.spill.Close()
	w.spill = nil
	removeErr := os.Remove(name)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}
