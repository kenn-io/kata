package teammate

import (
	"context"
	"net/http"
	"net/url"
)

// Header carries display contribution independently of comment addressing.
const Header = "X-Kata-Teammate"

type requestKey struct{}

// WithRequest freezes a request contribution. Empty explicitly clears a parent.
func WithRequest(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, requestKey{}, handle)
}

// FromRequest distinguishes an explicit empty value from no selection.
func FromRequest(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(requestKey{}).(string)
	return value, ok
}

// EditRequest applies the frozen selection only to mutations of the selected
// origin. It also strips inherited headers on reads and cross-origin redirects.
func EditRequest(ctx context.Context, req *http.Request, origin string) error {
	selected, err := url.Parse(origin)
	if err != nil {
		return err
	}
	mutation := req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch || req.Method == http.MethodDelete
	// Project resolution uses POST for its lookup inputs. It is also used by
	// reads and never contributes to an issue event, even when attaching an alias.
	if req.URL.Path == "/api/v1/projects/resolve" {
		mutation = false
	}
	if !mutation || req.URL.Scheme != selected.Scheme || req.URL.Host != selected.Host {
		req.Header.Del(Header)
		return nil
	}
	handle, present := FromRequest(ctx)
	if !present {
		return nil
	}
	if err := Validate(handle); err != nil {
		return err
	}
	req.Header.Del(Header)
	if handle != "" {
		req.Header.Set(Header, handle)
	}
	return nil
}

// WrapTransport prepares contribution once per transmitted HTTP hop. Reusing
// a decorated client replaces this owner rather than stacking preparation.
func WrapTransport(origin string, transport http.RoundTripper) http.RoundTripper {
	if existing, ok := transport.(requestTransport); ok {
		transport = existing.RoundTripper
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return requestTransport{RoundTripper: transport, origin: origin}
}

type requestTransport struct {
	http.RoundTripper
	origin string
}

func (t requestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	if err := EditRequest(request.Context(), request, t.origin); err != nil {
		return nil, err
	}
	return t.RoundTripper.RoundTrip(request)
}
