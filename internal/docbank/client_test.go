package docbank

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

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
	// SHA-256 of "abc". Docbank rejects a wrong hash or size.
	version, err := c.Upload(t.Context(), parent, "ba7816bf8f01-shot.png", strings.NewReader("abc"), 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil(), version)
	resp, err := c.Content(t.Context(), version)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "abc", string(b))
	require.EqualValues(t, 3, resp.ContentLength)
	_, err = c.Upload(t.Context(), parent, "ba7816bf8f01-bad.png", strings.NewReader("abc"), 4, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	var upstream *Error
	require.ErrorAs(t, err, &upstream)
	require.Equal(t, 422, upstream.Status)
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
