package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeStringFile is a test-only helper for building --body-file fixtures.
func writeStringFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// runEmitJSON invokes emitJSON against a fresh buffer and returns its output,
// removing the var-buf-then-String dance from each test case.
func runEmitJSON(t *testing.T, payload any) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := emitJSON(&buf, payload)
	return buf.String(), err
}

func TestResolveBody(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) (BodySources, io.Reader)
		want    string
		wantErr string
	}{
		{
			name: "flag wins",
			setup: func(*testing.T) (BodySources, io.Reader) {
				return BodySources{Body: "hello", BodySet: true}, nil
			},
			want: "hello",
		},
		{
			name: "explicit empty is honored",
			setup: func(*testing.T) (BodySources, io.Reader) {
				return BodySources{Body: "", BodySet: true}, nil
			},
			want: "",
		},
		{
			name: "from file",
			setup: func(t *testing.T) (BodySources, io.Reader) {
				path := t.TempDir() + "/b.txt"
				writeStringFile(t, path, "from file")
				return BodySources{File: path, FileSet: true}, nil
			},
			want: "from file",
		},
		{
			name: "from stdin",
			setup: func(*testing.T) (BodySources, io.Reader) {
				return BodySources{Stdin: true}, bytes.NewBufferString("from stdin")
			},
			want: "from stdin",
		},
		{
			name: "two sources is error",
			setup: func(*testing.T) (BodySources, io.Reader) {
				return BodySources{Body: "x", BodySet: true, Stdin: true}, nil
			},
			wantErr: "exactly one",
		},
		{
			// An explicit `--body ""` together with `--body-stdin` is still a
			// conflict — the previous (count by non-empty value) implementation
			// missed this case.
			name: "two sources with empty body is error",
			setup: func(*testing.T) (BodySources, io.Reader) {
				return BodySources{Body: "", BodySet: true, Stdin: true}, nil
			},
			wantErr: "exactly one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources, stdin := tc.setup(t)
			got, err := resolveBody(sources, stdin)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveActor_Precedence(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("KATA_AUTHOR", "env-shouldnt-win")
		t.Setenv("USER", "user-shouldnt-win")
		got, src := resolveActor(t.Context(), "flag-actor", func(context.Context) (string, error) { return "git-shouldnt-win", nil })
		assert.Equal(t, "flag-actor", got)
		assert.Equal(t, "flag", src)
	})
	t.Run("env wins when no flag", func(t *testing.T) {
		t.Setenv("KATA_AUTHOR", "env-actor")
		t.Setenv("USER", "user-shouldnt-win")
		got, src := resolveActor(t.Context(), "", func(context.Context) (string, error) { return "git-shouldnt-win", nil })
		assert.Equal(t, "env-actor", got)
		assert.Equal(t, "env", src)
	})
	t.Run("user wins when no flag or KATA_AUTHOR", func(t *testing.T) {
		t.Setenv("KATA_AUTHOR", "")
		t.Setenv("USER", "loginname")
		got, src := resolveActor(t.Context(), "", func(context.Context) (string, error) { return "git-shouldnt-win", nil })
		assert.Equal(t, "loginname", got)
		assert.Equal(t, "user", src)
	})
	t.Run("git wins when no flag, no env, no $USER", func(t *testing.T) {
		t.Setenv("KATA_AUTHOR", "")
		t.Setenv("USER", "")
		got, src := resolveActor(t.Context(), "", func(context.Context) (string, error) { return "git-user", nil })
		assert.Equal(t, "git-user", got)
		assert.Equal(t, "git", src)
	})
	t.Run("fallback when nothing else", func(t *testing.T) {
		t.Setenv("KATA_AUTHOR", "")
		t.Setenv("USER", "")
		got, src := resolveActor(t.Context(), "", func(context.Context) (string, error) { return "", nil })
		assert.Equal(t, "anonymous", got)
		assert.Equal(t, "fallback", src)
	})
}

func TestEmitJSON_AddsAPIVersion(t *testing.T) {
	out, err := runEmitJSON(t, map[string]string{"x": "y"})
	require.NoError(t, err)
	assert.Contains(t, out, `"kata_api_version":1`)
	assert.Contains(t, out, `"x":"y"`)
	assert.True(t, strings.HasSuffix(out, "\n"))
}

func TestEmitJSON_EmptyObject(t *testing.T) {
	out, err := runEmitJSON(t, struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "{\"kata_api_version\":1}\n", out)
}

func TestEmitJSON_RejectsNonObject(t *testing.T) {
	_, err := runEmitJSON(t, "scalar")
	require.Error(t, err)
	_, err = runEmitJSON(t, []int{1, 2, 3})
	require.Error(t, err)
	_, err = runEmitJSON(t, nil)
	require.Error(t, err)
}

func TestEmitJSON_RejectsReservedKey(t *testing.T) {
	_, err := runEmitJSON(t, map[string]any{"kata_api_version": "evil"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kata_api_version")
}

// A payload whose key is unicode-escaped must still be rejected. JSON
// permits "\uXXXX" escapes inside string content, so a payload like
// {"kata_api_version":"evil"} decodes to {"kata_api_version":"evil"}
// while the raw bytes do not contain the literal reserved key. A simple
// bytes.Contains(`"kata_api_version"`) guard would have let this slip
// through and produced a duplicate-keyed envelope downstream.
func TestEmitJSON_RejectsEscapedReservedKey(t *testing.T) {
	// Build the escape sequence explicitly — backtick raw strings interpret
	// the bytes literally, but writing `k` here avoids any rendering
	// ambiguity in the source file.
	payload := jsontext.Value([]byte(`{"\u006bata_api_version":"evil"}`))
	require.NotContains(t, string(payload), `"kata_api_version"`,
		"test fixture itself must contain the escape, not the literal key")
	_, err := runEmitJSON(t, payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kata_api_version")
}

// httpDoJSON sends a request body, returns (status, response body bytes).
func httpDoJSON(ctx context.Context, client *http.Client, method, url string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr) //nolint:gosec // daemon targets come from trusted routing; external refs are path-escaped
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	//nolint:gosec // G107: callers in cmd/kata/* always pass daemon-local URLs; this helper is package-internal.
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, bs, nil
}

func httpDoJSONHeaders(ctx context.Context, client *http.Client, method, path string, body any, headers map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rdr) //nolint:gosec // daemon targets come from trusted routing; external refs are path-escaped
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) //nolint:gosec // daemon-local URL, same as httpDoJSON.
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, bs, nil
}
