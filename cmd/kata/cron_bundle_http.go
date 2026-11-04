package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// cronBundleHTTP retains the selected daemon's transport, authentication and
// request budget, without applying generated current-write shapes to archives.
func cronBundleHTTP(ctx context.Context, a daemonAPI, method, path string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *a.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request) //nolint:gosec // Paths are built locally for the selected daemon; its existing transport pins bearer credentials to that origin.
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err == nil && response.StatusCode >= 300 && response.StatusCode < 400 {
		err = cronCLIValidation("cron bundle requests do not accept redirects")
	}
	return response.StatusCode, raw, err
}
