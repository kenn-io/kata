package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"go.kenn.io/kata/internal/cronbundle"
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

type cronBundleDaemon struct{ api daemonAPI }

func (d cronBundleDaemon) Read(ctx context.Context, projectID int64, kind, id string) (int, []byte, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/cron/%ss", projectID, kind)
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	return cronBundleHTTP(ctx, d.api, http.MethodGet, path, nil)
}
func (d cronBundleDaemon) Put(ctx context.Context, projectID int64, kind, id string, body cronbundle.PutBody) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return cronBundleHTTP(ctx, d.api, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/%ss/%s", projectID, kind, url.PathEscape(id)), raw)
}
func cronBundleCLIError(err error) error {
	if e, ok := errors.AsType[*cronbundle.ValidationError](err); ok {
		return cronCLIValidation(e.Message)
	}
	if e, ok := errors.AsType[*cronbundle.ResponseError](err); ok {
		return externalCLIResponseError(e.Status, e.Body, nil)
	}
	return err
}
