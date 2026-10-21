package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
)

func TestCLIEmptySearchConcurrentReceipt(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	stubIsTTY(t, true)
	cmd := findCommand(t, "search")
	target := client.ResolvedDaemon{BaseURL: "http://127.0.0.1:7777"}
	recordCLIUseTarget(target)
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, "/api/v1/ui/telemetry", r.URL.Path)
			if requests.Add(1) == 1 {
				close(started)
			}
			<-release
			return &http.Response{StatusCode: 202, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: r}, nil
		})
		var wg sync.WaitGroup
		wg.Go(func() { reportCLIEmptySearch(cmd, "missing") })
		<-started
		wg.Go(func() { reportCLIEmptySearch(cmd, "missing") })
		synctest.Wait()
		path := client.TelemetryDayMarkerPathForKey(client.TelemetryTargetKey(target), "cli-search-empty-")
		assert.Empty(t, client.ReadTelemetryDayMarker(path), "an in-flight send has no accepted receipt")
		close(release)
		wg.Wait()
		synctest.Wait()
		require.Equal(t, int32(1), requests.Load(), "concurrent invocations report one empty search")
		require.NotEmpty(t, client.ReadTelemetryDayMarker(path))
	})
}

func TestCLIEmptySearchRetriesFailedReceipt(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	stubIsTTY(t, true)
	cmd := findCommand(t, "search")
	recordCLIUseTarget(client.ResolvedDaemon{BaseURL: "http://127.0.0.1:7777"})
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	requests := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, errors.New("connection interrupted")
		}
		return &http.Response{StatusCode: 202, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: r}, nil
	})
	cmd.SetContext(context.Background())
	reportCLIEmptySearch(cmd, "missing")
	reportCLIEmptySearch(cmd, "missing")
	require.Equal(t, 2, requests, "a failed send leaves the query eligible")
}
