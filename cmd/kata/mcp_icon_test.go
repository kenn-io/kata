package main

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPServeHTTPPublicIcon(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveMCPTestHealth(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	session, err := newMCPHTTPTestClient().Connect(t.Context(), newAuthenticatedMCPHTTPTestTransport(endpoint), nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	assertMCPHTTPPublicIcon(t, http.DefaultClient, endpoint, "", session.InitializeResult().ServerInfo)
}

func assertMCPHTTPPublicIcon(t *testing.T, client *http.Client, endpoint, host string, info *sdkmcp.Implementation) {
	t.Helper()
	require.Equal(t, "https://katatracker.com", info.WebsiteURL)
	require.Len(t, info.Icons, 1)
	icon := info.Icons[0]
	require.Equal(t, "image/png", icon.MIMEType)
	require.Equal(t, []string{"256x256"}, icon.Sizes)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(icon.Source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(icon.Source, prefix))
	require.NoError(t, err)
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 256, img.Bounds().Dx())
	require.Equal(t, 256, img.Bounds().Dy())
	for _, path := range []string{"/icon.png", "/favicon.ico"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+path, func(t *testing.T) {
				request, err := http.NewRequestWithContext(t.Context(), method, strings.TrimSuffix(endpoint, "/mcp")+path, nil)
				require.NoError(t, err)
				request.Host = host
				request.Header.Set("Origin", "https://client.example")
				response, err := client.Do(request)
				require.NoError(t, err)
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Equal(t, "image/png", response.Header.Get("Content-Type"))
				require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
				require.Equal(t, "public, max-age=86400", response.Header.Get("Cache-Control"))
				require.Equal(t, strconv.Itoa(len(data)), response.Header.Get("Content-Length"))
				require.Empty(t, response.Header.Get("WWW-Authenticate"))
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				if method == http.MethodHead {
					require.Empty(t, body)
				} else {
					require.Equal(t, data, body)
				}
			})
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, nil)
	require.NoError(t, err)
	request.Host = host
	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, "public icons must not bypass MCP authentication")
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimSuffix(endpoint, "/mcp")+"/icon.png", nil)
	require.NoError(t, err)
	request.Host = "untrusted.example"
	response, err = client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden}, response.StatusCode, "public icons must retain listener Host validation")
}
