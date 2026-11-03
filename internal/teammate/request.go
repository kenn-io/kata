package teammate

import (
	"context"
	"net/http"

	"go.kenn.io/kata/internal/teammate/selection"
)

// Header carries display contribution independently of comment addressing.
const Header = selection.Header

// WithRequest freezes a request contribution. Empty explicitly clears a parent.
func WithRequest(ctx context.Context, handle string) context.Context {
	return selection.WithRequest(ctx, handle)
}

// FromRequest distinguishes an explicit empty value from no selection.
func FromRequest(ctx context.Context) (string, bool) {
	return selection.FromRequest(ctx)
}

// EditRequest applies the frozen selection only to mutations of the selected
// origin. It also strips inherited headers on reads and cross-origin redirects.
func EditRequest(ctx context.Context, req *http.Request, origin string) error {
	return selection.EditRequest(ctx, req, origin)
}

// WrapTransport prepares contribution once per transmitted HTTP hop. Reusing
// a decorated client replaces this owner rather than stacking preparation.
func WrapTransport(origin string, transport http.RoundTripper) http.RoundTripper {
	return selection.WrapTransport(origin, transport)
}
