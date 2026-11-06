package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"

	"go.kenn.io/kata/internal/api"
)

type contentReleaseValidatorKey struct{}

type contentReleaseValidator struct {
	validate func() error
}

// The federation handler adds its live transport check after authenticating
// the request. The outer writer owns the actual header/chunk release.
func setContentReleaseValidator(ctx context.Context, validate func() error) error {
	validator, _ := ctx.Value(contentReleaseValidatorKey{}).(*contentReleaseValidator)
	if validator == nil {
		return api.NewError(http.StatusServiceUnavailable, "authorization_unavailable", "content authorization is unavailable", "", nil)
	}
	validator.validate = validate
	return nil
}

// Content reads cannot wait for the complete body in the ordinary scoped
// response buffer. Recheck the same authority before releasing each chunk,
// and abort the transfer if that authority is lost after headers were sent.
func serveRevalidatedContent(w http.ResponseWriter, r *http.Request, next http.Handler, validate func() error) {
	response := &revalidatedContentResponse{underlying: w, header: make(http.Header), validate: validate}
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered != http.ErrAbortHandler || response.denied == nil || response.started {
				panic(recovered)
			}
			response.writeDenial()
		}
	}()
	next.ServeHTTP(response, r)
	// Body writes already checked authority inside the handler's lifetime.
	// Inner middleware can cancel its handler context when it returns.
	if !response.started {
		if err := response.release(); err != nil {
			response.writeDenial()
		}
	}
}

type revalidatedContentResponse struct {
	underlying http.ResponseWriter
	header     http.Header
	validate   func() error
	denied     error
	status     int
	started    bool
}

func (w *revalidatedContentResponse) Header() http.Header         { return w.header }
func (w *revalidatedContentResponse) Unwrap() http.ResponseWriter { return w.underlying }

func (w *revalidatedContentResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *revalidatedContentResponse) release() error {
	if w.denied != nil {
		return w.denied
	}
	if err := w.validate(); err != nil {
		w.denied = err
		return err
	}
	if !w.started {
		for key, values := range w.header {
			w.underlying.Header()[key] = append([]string(nil), values...)
		}
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		w.underlying.WriteHeader(status)
		w.started = true
	}
	return nil
}

func (w *revalidatedContentResponse) Write(body []byte) (int, error) {
	// Do not implement ReaderFrom: io.Copy must pass every chunk through this
	// check, including when the source offers an optimized transfer method.
	if len(body) == 0 {
		return 0, w.release()
	}
	written := 0
	for len(body) > 0 {
		if err := w.release(); err != nil {
			return written, err
		}
		chunk := body[:min(len(body), 32<<10)]
		n, err := w.underlying.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		body = body[n:]
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *revalidatedContentResponse) FlushError() error {
	if err := w.release(); err != nil {
		return err
	}
	return http.NewResponseController(w.underlying).Flush()
}

func (w *revalidatedContentResponse) writeDenial() {
	if apiErr, ok := errors.AsType[*api.APIError](w.denied); ok {
		api.WriteEnvelope(w.underlying, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	api.WriteEnvelope(w.underlying, http.StatusInternalServerError, "internal", "internal error")
}
