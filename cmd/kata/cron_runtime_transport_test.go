package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
	"hegel.dev/go/hegel"
)

// An attributed beat keeps its bytes and replay reader through SDK transport.
func TestCronRuntimeCLIWireBody(t *testing.T) {
	check := func(h require.TestingT, actor string) {
		// Keep materialized JSON within the 8 KiB request budget.
		if len(actor) > 1000 {
			actor = actor[:1000]
			for !utf8.ValidString(actor) {
				actor = actor[:len(actor)-1]
			}
		}
		raw, err := json.Marshal(map[string]string{"actor": "operator/" + actor, "label": "worker", "adapter": "example"})
		require.NoError(h, err)
		calls := 0
		client, err := kataclient.NewWithHTTPClient("http://127.0.0.1", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			require.Equal(h, "Bearer fixture-token", request.Header.Get("Authorization"))
			require.Equal(h, "application/json", request.Header.Get("Content-Type"))
			require.Equal(h, int64(len(raw)), request.ContentLength)
			body, err := io.ReadAll(request.Body)
			require.NoError(h, err)
			require.Equal(h, raw, body)
			require.NotNil(h, request.GetBody)
			for range 2 {
				replay, err := request.GetBody()
				require.NoError(h, err)
				body, err := io.ReadAll(replay)
				require.NoError(h, replay.Close())
				require.NoError(h, err)
				require.True(h, bytes.Equal(raw, body))
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}, kataclient.WithRequestEditor(func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer fixture-token")
			return nil
		}))
		require.NoError(h, err)
		_, err = (cronCLIOptions{resource: "runtime", action: "beat", identity: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}).callWrite(daemonAPI{ctx: t.Context()}, client, 1, raw)
		require.NoError(h, err)
		require.Equal(h, 1, calls)
	}
	t.Run("empty_actor_suffix", func(t *testing.T) { check(t, "") })
	t.Run("unicode", func(t *testing.T) {
		hegel.Test(t, func(h *hegel.T) { check(h, hegel.Draw(h, hegel.Text())) }, hegel.WithTestCases(200), hegel.WithDatabase(t.TempDir()))
	})
}
