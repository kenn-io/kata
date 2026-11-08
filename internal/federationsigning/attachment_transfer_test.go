package federationsigning

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yaronf/httpsign"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"hegel.dev/go/hegel"
)

const uploadPath = "/api/v1/projects/1/federation/docbank/issues/abc4/uploads"

func attachmentRequest(t *testing.T, source signingpolicy.Source, data []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example"+uploadPath, bytes.NewReader(data))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer enrollment")
	r.Header.Set("Content-Type", "application/octet-stream")
	require.NoError(t, Sign(r, source))
	original := r.Body
	t.Cleanup(func() { _ = original.Close(); _ = r.Body.Close() })
	return r
}

// Contract: binary uploads keep their media type and are usable after admission
// even when the body arrives after the ordinary signature lifetime.
func TestAttachmentTransferBinaryAndLateBody(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	data := []byte{0, 255, 1, 128, '\n'}
	r := attachmentRequest(t, s, data)
	require.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
	now := time.Now()
	v.now = func() time.Time { return now }
	r.Body = &advancingBody{Reader: strings.NewReader(string(data)), advance: func() { v.now = func() time.Time { return now.Add(5 * time.Minute) } }}
	require.NoError(t, v.Verify(r, 7))
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.Equal(t, data, body)
	_, ok := r.Body.(interface{ Stat() (os.FileInfo, error) })
	require.True(t, ok, "verified upload must use a disk spool")
}

// Contract: authenticated transfers consume a durable nonce before body I/O;
// failures and cancellation cannot reuse that signature.
func TestAttachmentTransferAdmissionBeforeBody(t *testing.T) {
	for _, mode := range []string{"digest", "cancel", "retire", "rotate", "success"} {
		t.Run(mode, func(t *testing.T) {
			s := testSource(t)
			v := testVerifier(t, "https://hub.example", s)
			r := attachmentRequest(t, s, []byte("binary"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r = r.WithContext(ctx)
			if mode == "retire" {
				k := v.keys[s.KeyID]
				k.NotAfter = time.Now().Add(time.Minute).Unix()
				v.keys[s.KeyID] = k
			}
			data := "binary"
			if mode == "digest" {
				data = "tamper"
			}
			r.Body = &advancingBody{Reader: strings.NewReader(data), advance: func() {
				require.Len(t, v.seen, 1, "nonce admission must precede body reading")
				state, err := os.ReadFile(v.statePath)
				require.NoError(t, err)
				require.NotEqual(t, "0\n", string(state), "admission must be durable")
				switch mode {
				case "cancel":
					cancel()
				case "retire":
					v.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
				case "rotate":
					t.Setenv(s.KeyEnv, strings.Repeat("b", 64))
				}
			}}
			err := v.Verify(r, 7)
			if mode == "success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, v.seen, 1)
			if mode == "success" {
				_ = r.Body.Close()
			}
			r = r.Clone(t.Context())
			r.Body = io.NopCloser(strings.NewReader("binary"))
			require.Error(t, v.Verify(r, 7), "consumed signature must not replay")
		})
	}
}

// Contract: a caller-configured external mount must not change binary profile.
func TestAttachmentTransferExternalMount(t *testing.T) {
	s := testSource(t)
	s.HubURL = "https://hub.example/federation"
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.HubURL+uploadPath, strings.NewReader("binary"))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer enrollment")
	r.Header.Set("Content-Type", "application/octet-stream")
	require.NoError(t, Sign(r, s))
	defer func() { _ = r.Body.Close() }()
	require.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
	r.URL.Path = uploadPath
	r.URL.Scheme, r.URL.Host = "", ""
	v := testVerifier(t, s.HubURL, s)
	require.NoError(t, v.Verify(r, 7))
	defer func() { _ = r.Body.Close() }()
}

// Contract: unknown length uploads become bounded, known-length file bodies.
func TestAttachmentTransferUnknownLengthAndSpoolCleanup(t *testing.T) {
	for _, length := range []int64{-1, 0} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			t.Setenv("TMP", dir)
			t.Setenv("TEMP", dir)
			s := testSource(t)
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example"+uploadPath, io.NopCloser(strings.NewReader("binary")))
			require.NoError(t, err)
			if length == -1 {
				r.ContentLength = -1
			} else {
				require.Zero(t, r.ContentLength, "generic client readers use zero for unknown length")
			}
			r.Header.Set("Authorization", "Bearer enrollment")
			r.Header.Set("Content-Type", "application/octet-stream")
			require.NoError(t, Sign(r, s))
			defer func() { _ = r.Body.Close() }()
			require.EqualValues(t, 6, r.ContentLength)
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, "binary", string(body))
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, files, 1)
			require.NoError(t, r.Body.Close())
			files, err = os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

// Contract: only a well-formed signed SHA-256 digest can reserve a transfer
// nonce or cause body reads, even when the caller owns a valid signing key.
func TestAttachmentTransferRejectsMalformedDigestBeforeBody(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	r := attachmentRequest(t, s, []byte("binary"))
	r.Header.Set("Content-Digest", "sha-256=:AA==:")
	key, err := s.Load()
	require.NoError(t, err)
	config := httpsign.NewSignConfig().SetKeyID(s.KeyID).SetExpiresAfter(90).SetNonce("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	signer, err := httpsign.NewHMACSHA256Signer(key, config, httpsign.Headers(components...))
	require.NoError(t, err)
	input, signature, err := httpsign.SignRequest("sig1", *signer, r)
	require.NoError(t, err)
	r.Header.Set("Signature-Input", input)
	r.Header.Set("Signature", signature)
	r.Body = &advancingBody{Reader: strings.NewReader("binary"), advance: func() { t.Fatal("invalid digest read body") }}
	require.Error(t, v.Verify(r, 7))
	require.Empty(t, v.seen)
}

// Contract: successful binary signing/verification is byte exact for arbitrary
// octets; corrupt bodies never become verified handler input.
func TestAttachmentTransferBinaryProperty(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		if len(data) > 64<<10 {
			data = data[:64<<10]
		} // materialization budget
		r := attachmentRequest(t, s, data)
		defer func() { _ = r.Body.Close() }()
		if err := v.Verify(r, 7); err != nil {
			ht.Fatalf("valid binary rejected: %v", err)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(data, body) {
			ht.Fatalf("binary changed: %v", err)
		}
	}, hegel.WithTestCases(64))
}

func TestAttachmentTransferCorruptBodyProperty(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		if len(data) > 64<<10 {
			data = data[:64<<10]
		}
		data = append(data, 0)
		r := attachmentRequest(t, s, data)
		_ = r.Body.Close()
		index := hegel.Draw(ht, hegel.Integers(0, len(data)-1))
		data[index] ^= 1
		r.Body = io.NopCloser(bytes.NewReader(data))
		if err := v.Verify(r, 7); err == nil {
			_ = r.Body.Close()
			ht.Fatal("corrupt binary accepted")
		}
	}, hegel.WithTestCases(64))
}

// Contract: body read failures and overflow leave no disk artifacts; cancellation
// must interrupt a body that is blocked waiting for the peer's next byte.
func TestAttachmentTransferSpoolFailureCleanup(t *testing.T) {
	for _, mode := range []string{"digest", "cancel", "read error", "overflow", "length mismatch", "transport"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			t.Setenv("TMP", dir)
			t.Setenv("TEMP", dir)
			s := testSource(t)
			v := testVerifier(t, "https://hub.example", s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := attachmentRequest(t, s, []byte("binary"))
			_ = r.Body.Close()
			r = r.WithContext(ctx)
			switch mode {
			case "digest":
				r.Body = io.NopCloser(strings.NewReader("tamper"))
			case "cancel":
				reader, writer := io.Pipe()
				defer func() { _ = writer.Close() }()
				r.Body = reader
				started := make(chan struct{})
				r.Body = &signingReadSignal{ReadCloser: reader, started: started}
				done := make(chan error, 1)
				go func() { done <- v.Verify(r, 7) }()
				<-started
				cancel()
				select {
				case err := <-done:
					require.Error(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled spool stayed blocked")
				}
			case "read error":
				r.Body = io.NopCloser(signingErrorReader{})
			case "overflow":
				r.ContentLength = 256<<20 + 1
			case "length mismatch":
				r.Body = io.NopCloser(strings.NewReader("short"))
			case "transport":
				r.Body = io.NopCloser(strings.NewReader("binary"))
				client := &http.Client{Transport: signingRoundTripFunc(func(_ *http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })}
				require.NoError(t, ConfigureClient(client, "https://hub.example", "enrollment", s))
				_, err := client.Do(r)
				require.Error(t, err)
			}
			if mode != "cancel" && mode != "transport" {
				require.Error(t, v.Verify(r, 7))
			}
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

type signingErrorReader struct{}

func (signingErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type signingReadSignal struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *signingReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

type signingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f signingRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Contract: maximum attachments exceed ordinary signing limits without a
// file-sized heap allocation; exactly the cap succeeds, one extra byte fails.
func TestAttachmentTransferLargeBoundedBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	for _, size := range []int64{128<<20 + 1, 256 << 20, 256<<20 + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example"+uploadPath, io.NopCloser(io.LimitReader(signingZeroReader{}, size)))
			require.NoError(t, err)
			r.ContentLength = -1 // exercise actual EOF/overflow, never trust the header
			r.Header.Set("Authorization", "Bearer enrollment")
			r.Header.Set("Content-Type", "application/octet-stream")
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err = Sign(r, s)
			if size > 256<<20 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				sent := r.Body
				require.NoError(t, v.Verify(r, 7))
				require.NoError(t, sent.Close())
				n, err := io.Copy(io.Discard, r.Body)
				require.NoError(t, err)
				require.Equal(t, size, n)
				require.NoError(t, r.Body.Close())
			}
			runtime.ReadMemStats(&after)
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20), "heap allocations must not scale with file size")
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

type signingZeroReader struct{}

func (signingZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestAttachmentTransferRejectsInFlightReplayBeforeBody(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	r := attachmentRequest(t, s, []byte("binary"))
	_ = r.Body.Close()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	started := make(chan struct{})
	r.Body = &signingReadSignal{ReadCloser: reader, started: started}
	done := make(chan error, 1)
	go func() { done <- v.Verify(r, 7) }()
	<-started
	duplicate := r.Clone(t.Context())
	duplicate.Body = &advancingBody{Reader: strings.NewReader("binary"), advance: func() { t.Error("replay read body") }}
	require.Error(t, v.Verify(duplicate, 7))
	_, err := writer.Write([]byte("binary"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, <-done)
	require.NoError(t, r.Body.Close())
}

func TestAttachmentTransferAuthenticatesBeforeBody(t *testing.T) {
	for _, mode := range []string{"MAC", "expired", "enrollment", "watermark failure"} {
		t.Run(mode, func(t *testing.T) {
			s := testSource(t)
			v := testVerifier(t, "https://hub.example", s)
			r := attachmentRequest(t, s, []byte("binary"))
			r.Body = &advancingBody{Reader: strings.NewReader("binary"), advance: func() { t.Fatal("rejected headers read body") }}
			enrollmentID := int64(7)
			switch mode {
			case "MAC":
				r.Header.Set("Authorization", "Bearer tampered")
			case "expired":
				v.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			case "enrollment":
				enrollmentID++
			case "watermark failure":
				v.statePath = filepath.Join(t.TempDir(), "missing", "state")
			}
			require.Error(t, v.Verify(r, enrollmentID))
			require.Empty(t, v.seen)
		})
	}
}

func TestAttachmentTransferRestartRetainsAdmissionBarrier(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	r := attachmentRequest(t, s, []byte("binary"))
	r.Body = &advancingBody{Reader: strings.NewReader("tamper"), advance: func() {}}
	require.Error(t, v.Verify(r, 7))
	require.NoError(t, v.Close())
	restarted, err := NewVerifier("https://hub.example", []signingpolicy.Key{{Source: s, EnrollmentID: 7}}, v.statePath)
	require.NoError(t, err)
	defer func() { _ = restarted.Close() }()
	// Simulate an elapsed quarantine but a persisted future expiry barrier.
	restarted.started = time.Now().Add(-Quarantine)
	r.Body = &advancingBody{Reader: strings.NewReader("binary"), advance: func() { t.Fatal("restart replay read body") }}
	require.ErrorIs(t, restarted.Verify(r, 7), ErrWarming)
}

func TestAttachmentTransferContentGETRetainsOrdinaryBodyCap(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.example/api/v1/projects/1/federation/docbank/versions/6f1c2e8a-2222-4222-8222-222222222222/content", nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer enrollment")
	require.NoError(t, Sign(r, s))
	r.ContentLength = MaxBodyBytes + 1
	r.Body = &advancingBody{Reader: strings.NewReader(""), advance: func() { t.Fatal("GET accepted upload body cap") }}
	require.Error(t, v.Verify(r, 7))
}
