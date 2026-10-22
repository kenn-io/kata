package docbank

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func TestDocbankUploadAndContent(t *testing.T) {
	f := docbanktest.New(t)
	c := NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	parent, err := c.EnsureFolder(t.Context(), "/kata/project-a")
	require.NoError(t, err)
	same, err := c.EnsureFolder(t.Context(), "/kata/project-a")
	require.NoError(t, err)
	require.Equal(t, parent, same)
	// SHA-256 of "abc".
	version, err := c.Upload(t.Context(), parent, "shot.png", strings.NewReader("abc"), 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil(), version)
	resp, err := c.Content(t.Context(), version)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "abc", string(b))
	require.EqualValues(t, 3, resp.ContentLength)
}
func TestDocbankContentWithoutTrailer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Docbank-Blob-Hash", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
		_, _ = io.WriteString(w, "abc")
	}))
	defer upstream.Close()
	c := NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
	resp, err := c.Content(t.Context(), uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "abc", string(b))
}

func TestDocbankContentRejectsCorruptContent(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong digest", true: "missing digest"}[missing], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/versions/6f1c2e8a-2222-4222-8222-222222222222/content", r.URL.Path)
				w.Header().Set("X-Docbank-Blob-Size", "4")
				if !missing {
					// SHA-256 of "AAAA".
					w.Header().Set("X-Docbank-Blob-Hash", "63c1dd951ffedf6f7fd968ad4efa39b8ed584f162f46e715114ee184f8de9201")
				}
				_, _ = io.WriteString(w, "BBBB")
			}))
			defer upstream.Close()
			c := NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
			resp, err := c.Content(t.Context(), uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222"))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			require.EqualError(t, err, "docbank: content digest mismatch")
			require.Empty(t, b, "the final chunk must be withheld")
		})
	}
}

func TestDocbankUploadRejectsMissingVersion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/uploads", r.URL.Path)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"node":{}}`)
	}))
	defer upstream.Close()
	c := NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
	version, err := c.Upload(t.Context(), 1, "sample.txt", strings.NewReader("abc"), 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	require.Error(t, err)
	require.Equal(t, uuid.Nil(), version)
}
func TestDocbankStatusAndKeyRotation(t *testing.T) {
	key := "first"
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Api-Key"))
		http.Error(w, "private upstream detail", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := NewClient(server.URL, func() (string, error) { return key, nil })
	for _, value := range []string{"first", "rotated"} {
		key = value
		_, err := c.Content(t.Context(), uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222"))
		var upstream *Error
		require.ErrorAs(t, err, &upstream)
		require.Equal(t, 503, upstream.Status)
		require.Equal(t, "docbank: HTTP 503", err.Error())
	}
	require.Equal(t, []string{"first", "rotated"}, seen)
}
func TestDocbankRefusesRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	c := NewClient(server.URL, func() (string, error) { return "test-key", nil })
	_, err := c.Content(t.Context(), uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222"))
	require.Error(t, err)
	require.Zero(t, calls)
}
