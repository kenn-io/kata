package daemon

import (
	"bytes"
	"context"
	"net/http"

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
}

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
	return w.body.Write(body)
}

func (w *bufferedScopedResponse) writeTo(destination http.ResponseWriter) {
	for key, values := range w.header {
		destination.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(http.StatusOK)
	destination.WriteHeader(w.status)
	_, _ = destination.Write(w.body.Bytes())
}
