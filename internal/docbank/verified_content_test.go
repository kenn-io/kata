package docbank_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank"
	"hegel.dev/go/hegel"
)

type verificationBody struct {
	io.Reader
	closed bool
}

func (b *verificationBody) Close() error { b.closed = true; return nil }

func contentFixture(data []byte, size, digest string) (*http.Response, *verificationBody) {
	body := &verificationBody{Reader: bytes.NewReader(data)}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Docbank-Blob-Size": {size}, "X-Docbank-Blob-Hash": {digest}}, Body: body, ContentLength: -1}, body
}
func digestOf(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Forwarded streams must prove both identity and complete length. A matching
// hash alone cannot prove that a caller's reported byte count is truthful.
func TestForwardedDocbankContentVerifiesLengthAndDigest(t *testing.T) {
	for _, tc := range []struct {
		name, size, digest string
		data               []byte
		valid              bool
	}{
		{"valid", "3", digestOf([]byte("abc")), []byte("abc"), true},
		{"empty", "0", digestOf(nil), nil, true},
		{"empty wrong hash", "0", digestOf([]byte("x")), nil, false},
		{"nonempty claimed zero", "0", digestOf([]byte("x")), []byte("x"), false},
		{"missing size", "", digestOf([]byte("abc")), []byte("abc"), false},
		{"negative size", "-1", digestOf([]byte("abc")), []byte("abc"), false},
		{"bad size", "3bytes", digestOf([]byte("abc")), []byte("abc"), false},
		{"maximum size short", "9223372036854775807", digestOf([]byte("abc")), []byte("abc"), false},
		{"overflow", "9223372036854775808", digestOf([]byte("abc")), []byte("abc"), false},
		{"truncated but hash matches", "4", digestOf([]byte("abc")), []byte("abc"), false},
		{"overlong but hash matches", "2", digestOf([]byte("abc")), []byte("abc"), false},
		{"missing hash", "3", "", []byte("abc"), false},
		{"bad hash", "3", strings.Repeat("z", 64), []byte("abc"), false},
		{"wrong hash", "3", digestOf([]byte("xyz")), []byte("abc"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := contentFixture(tc.data, tc.size, tc.digest)
			verified, err := docbank.VerifyContentResponse(resp)
			var got []byte
			if err == nil {
				got, err = io.ReadAll(verified.Body)
				require.NoError(t, verified.Body.Close())
			}
			require.True(t, body.closed, "every returned or refused body must retire")
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, string(tc.data), string(got))
			} else {
				require.Error(t, err)
				require.Empty(t, got, "a small invalid stream must withhold its final chunk")
			}
		})
	}
}

// The complete content space and false size are independent. This catches
// validators that only compare a declared length with another declared length.
func TestForwardedDocbankContentCountProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		size := hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
		// The sole true size is mapped to its adjacent false value; every other
		// nonnegative int64 remains in this rejection domain.
		if size == int64(len(data)) {
			size ^= 1
		}
		resp, body := contentFixture(data, strconv.FormatInt(size, 10), digestOf(data))
		verified, err := docbank.VerifyContentResponse(resp)
		if err == nil {
			_, err = io.Copy(io.Discard, verified.Body)
			_ = verified.Body.Close()
		}
		if err == nil || !body.closed {
			ht.Fatalf("false size accepted or body leaked: bytes=%d size=%d closed=%t", len(data), size, body.closed)
		}
	}, hegel.WithTestCases(2048))
}

type fragmentedContent struct {
	reader       *bytes.Reader
	chunk        int
	eofWithData  bool
	emptyReads   bool
	emptyPending bool
}

func (f *fragmentedContent) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.emptyReads && !f.emptyPending {
		f.emptyPending = true
		return 0, nil
	}
	f.emptyPending = false
	n, err := f.reader.Read(p[:min(len(p), f.chunk)])
	if f.eofWithData && f.reader.Len() == 0 {
		return n, io.EOF
	}
	return n, err
}

type contentWriterOnly struct{ io.Writer }

func TestForwardedDocbankContentRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		chunk := hegel.Draw(ht, hegel.Integers(1, 8192))
		digest := []byte(digestOf(data))
		caseMask := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		for i, digit := range digest {
			if caseMask&(uint64(1)<<i) != 0 && digit >= 'a' && digit <= 'f' {
				digest[i] -= 'a' - 'A'
			}
		}
		resp, body := contentFixture(data, strconv.Itoa(len(data)), string(digest))
		body.Reader = &fragmentedContent{reader: bytes.NewReader(data), chunk: chunk,
			eofWithData: hegel.Draw(ht, hegel.Integers(0, 1)) == 1,
			emptyReads:  hegel.Draw(ht, hegel.Integers(0, 1)) == 1,
		}
		verified, err := docbank.VerifyContentResponse(resp)
		if err != nil {
			ht.Fatalf("valid response rejected: %v", err)
		}
		if n, err := verified.Body.Read(nil); n != 0 || err != nil && err != io.EOF {
			ht.Fatalf("zero-length read changed or rejected valid content: %d %v", n, err)
		}
		consumerChunk := hegel.Draw(ht, hegel.Integers(1, 65536))
		var got bytes.Buffer
		_, err = io.CopyBuffer(contentWriterOnly{Writer: &got}, verified.Body, make([]byte, consumerChunk))
		closeErr := verified.Body.Close()
		if err != nil || closeErr != nil || !bytes.Equal(data, got.Bytes()) || !body.closed {
			ht.Fatalf("content roundtrip failed for source=%d sink=%d bytes=%d: %v %v", chunk, consumerChunk, len(data), err, closeErr)
		}
	}, hegel.WithTestCases(2048))
}

// A reader may stop once Content-Length bytes arrive. Detect lookahead excess
// before releasing that final chunk, rather than waiting for a later EOF read.
func TestForwardedDocbankContentWithholdsDeclaredFinalChunkOnExcess(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 32769)
	resp, _ := contentFixture(data, "32768", digestOf(data))
	verified, err := docbank.VerifyContentResponse(resp)
	require.NoError(t, err)
	defer func() { require.NoError(t, verified.Body.Close()) }()
	n, err := io.ReadFull(verified.Body, make([]byte, 32768))
	require.Error(t, err)
	require.Less(t, n, 32768, "an overlong response must not publish a complete declared body")
}

// generatedContent creates bytes on demand and lets this test observe whether
// the verifier buffers the entire file before returning its first chunk.
type generatedContent struct {
	remaining, produced int64
	closed              bool
}

func (g *generatedContent) Read(p []byte) (int, error) {
	if g.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), g.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	g.remaining -= n
	g.produced += n
	return int(n), nil
}
func (g *generatedContent) Close() error { g.closed = true; return nil }

func TestForwardedDocbankContentStreamsLargeGeneratedBody(t *testing.T) {
	const size = 64 << 20
	body := &generatedContent{remaining: size}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Docbank-Blob-Size": {"67108864"}, "X-Docbank-Blob-Hash": {"e20a69eca39368572e90b9135738a613838f954987a0b44b6220889c171cbb76"}}, Body: body, ContentLength: -1}
	verified, err := docbank.VerifyContentResponse(resp)
	require.NoError(t, err)
	first := make([]byte, 8)
	n, err := io.ReadFull(verified.Body, first)
	require.NoError(t, err)
	require.Equal(t, "xxxxxxxx", string(first))
	require.LessOrEqual(t, body.produced, int64(65536), "first chunk must not await full file buffering")
	remaining, err := io.Copy(io.Discard, verified.Body)
	require.NoError(t, err)
	require.Equal(t, int64(size), int64(n)+remaining)
	require.NoError(t, verified.Body.Close())
	require.True(t, body.closed)
}

func TestForwardedDocbankContentRejectsHeadersBeforeBody(t *testing.T) {
	for _, tc := range []struct{ size, digest string }{
		{"", digestOf(nil)}, {"-1", digestOf(nil)}, {"bogus", digestOf(nil)}, {"9223372036854775808", digestOf(nil)}, {"0", ""}, {"0", strings.Repeat("z", 64)}, {"0", strings.Repeat("a", 63)},
	} {
		t.Run(tc.size+"-"+tc.digest, func(t *testing.T) {
			resp, body := contentFixture([]byte("unread fixture"), tc.size, tc.digest)
			_, err := docbank.VerifyContentResponse(resp)
			require.Error(t, err, "invalid headers must be rejected before returning a stream")
			require.True(t, body.closed, "refused response must retire immediately")
			// Exercise the public HTTP client sibling with the identical hostile
			// headers, so falling back to legacy Content cannot hide rejection.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Docbank-Blob-Size", tc.size)
				w.Header().Set("X-Docbank-Blob-Hash", tc.digest)
				_, _ = w.Write([]byte("unread fixture"))
			}))
			defer server.Close()
			client := docbank.NewClient(server.URL, func() (string, error) { return "test-key", nil })
			response, err := client.OriginContent(t.Context(), uuid.MustParse("6f1c2e8a-4444-4444-8444-444444444419"))
			if response != nil {
				defer func() { _ = response.Body.Close() }()
			}
			require.Error(t, err, "OriginContent must agree with the strict verifier")
		})
	}
}

// Any single-bit mutation changes content identity, even when both the body
// length and the digest header remain well formed.
func TestForwardedDocbankContentRejectsMutationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		first := hegel.Draw(ht, hegel.Integers[uint8](0, 255))
		data := append([]byte{first}, hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))...)
		digest := digestOf(data)
		position := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		bit := hegel.Draw(ht, hegel.Integers[uint8](0, 7))
		data[position%uint64(len(data))] ^= 1 << bit
		resp, body := contentFixture(data, strconv.Itoa(len(data)), digest)
		verified, err := docbank.VerifyContentResponse(resp)
		if err != nil {
			ht.Fatalf("well-formed headers rejected before reading the body: %v", err)
		}
		got, err := io.ReadAll(verified.Body)
		closeErr := verified.Body.Close()
		if err == nil || len(got) >= len(data) || closeErr != nil || !body.closed {
			ht.Fatalf("mutated content completed: bytes=%d released=%d err=%v close=%v", len(data), len(got), err, closeErr)
		}
	}, hegel.WithTestCases(2048))
}

type terminalErrorContent struct {
	reader *bytes.Reader
	err    error
}

func (r terminalErrorContent) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF || r.reader.Len() == 0 {
		return n, r.err
	}
	return n, err
}

// A source can fail together with its final bytes. Matching count and digest
// must not turn that transport failure into a complete successful file.
func TestForwardedDocbankContentRejectsReaderErrorProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		interrupted := errors.New("synthetic content interruption")
		resp, body := contentFixture(data, strconv.Itoa(len(data)), digestOf(data))
		body.Reader = terminalErrorContent{reader: bytes.NewReader(data), err: interrupted}
		verified, err := docbank.VerifyContentResponse(resp)
		if err != nil {
			ht.Fatalf("valid headers rejected: %v", err)
		}
		got, err := io.ReadAll(verified.Body)
		closeErr := verified.Body.Close()
		if !errors.Is(err, interrupted) || len(data) > 0 && len(got) >= len(data) || closeErr != nil || !body.closed {
			ht.Fatalf("source error lost or completed: bytes=%d released=%d err=%v close=%v", len(data), len(got), err, closeErr)
		}
	}, hegel.WithTestCases(2048))
}
