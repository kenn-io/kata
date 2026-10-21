package docbank

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
	"pgregory.net/rapid"
)

func uploadFixture(t *testing.T, f *docbanktest.Fake) (*Client, metadata.Attachment) {
	t.Helper()
	c, err := NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	ctx := t.Context()
	vault, err := c.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, docbanktest.VaultUID, vault)
	parent, err := c.EnsureFolder(ctx, "/kata/project-a")
	require.NoError(t, err)
	b := []byte("original content")
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	receipt, err := c.Upload(ctx, parent, "content.txt", "text/plain", bytes.NewReader(b), int64(len(b)), hash)
	require.NoError(t, err)
	return c, metadata.Attachment{Name: "content.txt", MediaType: "text/plain", Size: int64(len(b)), Docbank: metadata.DocbankReference{VaultUID: vault, NodeID: receipt.Node.ID, ContentVersionID: receipt.Node.VersionID, ContentSHA256: hash}}
}
func TestContentPinnedIntegrity(t *testing.T) {
	for _, fault := range []string{"", "short", "bytes", "trailer", "missing"} {
		t.Run(fault, func(t *testing.T) {
			f := docbanktest.New(t)
			c, e := uploadFixture(t, f)
			f.Fault = fault
			stream, err := c.Content(t.Context(), e)
			if fault == "missing" {
				require.Equal(t, "not_found", ErrorKind(err))
				return
			}
			require.NoError(t, err)
			b, err := io.ReadAll(stream)
			closeErr := stream.Close()
			if fault == "" {
				require.NoError(t, err)
				require.NoError(t, closeErr)
				require.Equal(t, "original content", string(b))
			} else {
				require.Equal(t, "integrity", ErrorKind(err))
				require.Equal(t, "integrity", ErrorKind(closeErr))
			}
		})
	}
}
func TestTextStatesAndPinnedVersion(t *testing.T) {
	for _, state := range []string{"ready", "verified_empty", "unprocessed", "failed", "unconfigured", "profile_required", "historical_unavailable"} {
		t.Run(state, func(t *testing.T) {
			f := docbanktest.New(t)
			c, e := uploadFixture(t, f)
			f.State = state
			out, err := c.Text(t.Context(), e, "ocr", 5)
			require.NoError(t, err)
			if state == "verified_empty" {
				require.Equal(t, "ready", out.State)
				require.Empty(t, out.Text)
			} else {
				require.Equal(t, state, out.State)
			}
			if state == "ready" {
				require.Equal(t, "OCR c", out.Text)
				require.True(t, out.Truncated)
			}
		})
	}
}
func TestClientErrorClassesAndRedirectRefusal(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
	}{{401, "unauthorized"}, {403, "unauthorized"}, {404, "not_found"}, {413, "too_large"}, {429, "upstream"}, {503, "upstream"}} {
		t.Run(tc.kind+http.StatusText(tc.status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer s.Close()
			c, err := NewClient(s.URL, func() (string, error) { return "test-key", nil })
			require.NoError(t, err)
			_, err = c.Info(t.Context())
			require.Equal(t, tc.kind, ErrorKind(err))
		})
	}
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls++ }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer s.Close()
	c, _ := NewClient(s.URL, func() (string, error) { return "test-key", nil })
	_, err := c.Info(t.Context())
	require.Error(t, err)
	require.Zero(t, calls)
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"vault_id":"`+docbanktest.VaultUID+`"}`+strings.Repeat(" ", 1<<20))
	}))
	defer s2.Close()
	c, _ = NewClient(s2.URL, func() (string, error) { return "test-key", nil })
	_, err = c.Info(t.Context())
	require.Equal(t, "invalid_receipt", ErrorKind(err))
}
func TestInvalidUploadReceipt(t *testing.T) {
	f := docbanktest.New(t)
	f.Fault = "receipt"
	c, _ := NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	sum := sha256.Sum256([]byte("x"))
	_, err := c.Upload(t.Context(), 1, "x.txt", "text/plain", strings.NewReader("x"), 1, hex.EncodeToString(sum[:]))
	require.Equal(t, "invalid_receipt", ErrorKind(err))
}
func TestTruncateUTF8Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "text")
		n := rapid.IntRange(0, len(s)+4).Draw(t, "maxBytes")
		got := truncateUTF8([]byte(s), n)
		require.True(t, utf8.Valid(got))
		require.LessOrEqual(t, len(got), n)
		require.True(t, strings.HasPrefix(s, string(got)))
	})
}

func FuzzTruncateUTF8(f *testing.F) {
	f.Add("OCR café", uint16(6))
	f.Add("", uint16(0))
	f.Add("😀界", uint16(4))
	f.Fuzz(func(t *testing.T, s string, n uint16) {
		if !utf8.ValidString(s) {
			t.Skip("rendition text contract requires UTF-8")
		}
		got := truncateUTF8([]byte(s), int(n))
		require.True(t, utf8.Valid(got))
		require.LessOrEqual(t, len(got), int(n))
		require.True(t, strings.HasPrefix(s, string(got)))
	})
}

func TestTextLargeArtifactTruncatesAfterVerification(t *testing.T) {
	f := docbanktest.New(t)
	c, e := uploadFixture(t, f)
	f.State = "ready"
	f.Text = strings.Repeat("é", (8<<20)+1)
	out, err := c.Text(t.Context(), e, "ocr", 5)
	require.NoError(t, err)
	require.Equal(t, "éé", out.Text)
	require.True(t, out.Truncated)
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }
func TestContentTransportInterruptionReturnsUnreachable(t *testing.T) {
	stream := &verifiedStream{resp: &http.Response{Body: failingBody{}}, h: sha256.New(), cancel: func() {}}
	_, err := io.ReadAll(stream)
	require.Equal(t, "unreachable", ErrorKind(err))
	require.Equal(t, err, stream.Close())
}

func TestTextPrefixChunkIndependence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := rapid.String().Draw(t, "text")
		limit := rapid.IntRange(1, 16<<20).Draw(t, "max_bytes")
		chunk := rapid.IntRange(1, 257).Draw(t, "chunk_bytes")
		p := &textPrefix{limit: limit, valid: true}
		for rest := []byte(text); len(rest) > 0; {
			n := min(chunk, len(rest))
			_, _ = p.Write(rest[:n])
			rest = rest[n:]
		}
		require.True(t, p.valid)
		require.Empty(t, p.tail)
		boundary := len(text)
		if boundary > limit {
			boundary = 0
			for i := range text {
				if i <= limit {
					boundary = i
				}
			}
		}
		require.Equal(t, text[:boundary], string(truncateUTF8(p.bytes, limit)))
	})
}
