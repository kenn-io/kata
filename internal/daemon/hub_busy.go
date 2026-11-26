package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
)

type hubBusyKey struct{}
type hubBusyTransport struct{ next http.RoundTripper }

func observeHubBusy(c *http.Client) {
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c.Transport = hubBusyTransport{next: transport}
}

func (t hubBusyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || req.Context().Value(hubBusyKey{}) == nil {
		return resp, err
	}
	message, _, busy := api.InspectBusyResponse(resp, req.URL.Hostname())
	if !busy {
		return resp, nil
	}
	_ = resp.Body.Close()
	failure := api.NewError(http.StatusTooManyRequests, api.CodeServerBusy, message, "", nil)
	if hint := resp.Header.Get("Retry-After"); hint != "" {
		return nil, huma.ErrorWithHeaders(failure, http.Header{"Retry-After": []string{hint}})
	}
	return nil, failure
}

func withHubBusy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hubBusyKey{}, true)))
	})
}

func forwardedHubBusy(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests && apiErr.Code == api.CodeServerBusy {
		return err
	}
	return nil
}

func syncHubBusy(err error, hubURL string) error {
	var status interface {
		HTTPStatus() int
		HTTPBody() string
		HTTPRetryAfter() time.Duration
	}
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusTooManyRequests {
		return nil
	}
	hub, err := url.Parse(hubURL)
	if err != nil {
		return nil
	}
	message, _, busy := api.BusyRefusal([]byte(status.HTTPBody()), hub.Hostname())
	if !busy {
		return nil
	}
	failure := api.NewError(http.StatusTooManyRequests, api.CodeServerBusy, message, "", nil)
	if status.HTTPRetryAfter() > 0 {
		seconds := (status.HTTPRetryAfter() + time.Second - 1) / time.Second
		return huma.ErrorWithHeaders(failure, http.Header{"Retry-After": []string{strconv.FormatInt(int64(seconds), 10)}})
	}
	return failure
}
