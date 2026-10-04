package federationsigning

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testSource(t *testing.T) Source {
	t.Helper()
	t.Setenv("KATA_TEST_SIGNING_KEY", strings.Repeat("a", 64))
	return Source{KeyID: "spoke-key", KeyEnv: "KATA_TEST_SIGNING_KEY"}
}

func testVerifier(t *testing.T, base string, source Source) *Verifier {
	t.Helper()
	p := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, InitializeReplayState(p))
	v, err := NewVerifier(base, []Key{{Source: source, EnrollmentID: 7}}, p)
	t.Cleanup(func() {
		if v != nil {
			_ = v.Close()
		}
	})
	require.NoError(t, err)
	v.started = time.Now().Add(-Quarantine)
	return v
}

func TestSourceLoadsOwnerOnlyKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	secret := strings.Repeat("k", 64)
	require.NoError(t, os.WriteFile(path, []byte(secret), 0600))

	got, err := (Source{KeyID: "key-a", KeyFile: path}).Load()
	require.NoError(t, err)
	require.Equal(t, []byte(secret), got)
}

func TestSourceRejectsNonRegularKeyFile(t *testing.T) {
	dir := t.TempDir()

	got, err := (Source{KeyID: "key-a", KeyFile: dir}).Load()
	require.ErrorIs(t, err, ErrKey)
	require.Nil(t, got)
}

func TestSourceRejectsKeyFileReadableByOthersOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission contract")
	}
	path := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("k", 64)), 0600))
	require.NoError(t, os.Chmod(path, 0644)) // #nosec G302 -- intentionally makes the test key readable to prove rejection.

	got, err := (Source{KeyID: "key-a", KeyFile: path}).Load()
	require.ErrorIs(t, err, ErrKey)
	require.Nil(t, got)
}

func signed(t *testing.T, source Source) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "https://hub.example/federation/api/v1/projects/1/federation/events:ingest?limit=2", strings.NewReader(`{"events":[]}`))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer enrollment")
	r.Header.Set("Content-Type", "application/json")
	require.NoError(t, Sign(r, source))
	return r
}

// Contract: method, exact public target, query, digest and bearer are signed.
func TestTamperingAndPrefix(t *testing.T) {
	s := testSource(t)
	for _, change := range []string{"none", "method", "path", "query", "body", "content-type", "bearer", "duplicate", "encoded", "forwarded"} {
		t.Run(change, func(t *testing.T) {
			r := signed(t, s)
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/federation")
			r.URL.Scheme, r.URL.Host = "", ""
			switch change {
			case "method":
				r.Method = http.MethodGet
			case "path":
				r.URL.Path += "/other"
			case "query":
				r.URL.RawQuery = "limit=3"
			case "body":
				r.Body = io.NopCloser(strings.NewReader(`{"events":[1]}`))
			case "content-type":
				r.Header.Set("Content-Type", "text/plain")
			case "bearer":
				r.Header.Set("Authorization", "Bearer different")
			case "duplicate":
				r.Header.Add("Signature", r.Header.Get("Signature"))
			case "encoded":
				r.URL.RawPath = strings.Replace(r.URL.Path, "projects", "%70rojects", 1)
			case "forwarded":
				r.Header.Set("X-Forwarded-Host", "attacker.example")
				r.Header.Set("X-Forwarded-Prefix", "/other")
			}
			v := testVerifier(t, "https://hub.example/federation", s)
			err := v.Verify(r, 7)
			if change == "none" || change == "forwarded" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// Contract: replay admission is atomic and bounded; restarting fails closed.
func TestReplayConcurrencyRestartAndPressure(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	r := signed(t, s)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			cloned := r.Clone(t.Context())
			cloned.Body = io.NopCloser(strings.NewReader(`{"events":[]}`))
			results <- v.Verify(cloned, 7)
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	p := filepath.Join(t.TempDir(), "restart.state")
	require.NoError(t, InitializeReplayState(p))
	restarted, err := NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, p)
	t.Cleanup(func() {
		if restarted != nil {
			_ = restarted.Close()
		}
	})
	require.NoError(t, err)
	require.ErrorIs(t, restarted.Verify(signed(t, s), 7), ErrWarming)
	v.capacity = 1
	require.ErrorIs(t, v.Verify(signed(t, s), 7), ErrCapacity)
}

func TestSecretSourcesScopeAndBodyLimit(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	require.Error(t, v.Verify(signed(t, s), 8))
	r := signed(t, s)
	r.Body = io.NopCloser(bytes.NewReader(make([]byte, MaxBodyBytes+1)))
	require.Error(t, v.Verify(r, 7))
	t.Setenv(s.KeyEnv, "")
	require.Error(t, Sign(signedWithoutKey(t), s))
	require.Error(t, v.Verify(signedWithoutKey(t), 7))
}

func TestVerifierPreservesRequestBodyLimitError(t *testing.T) {
	source := testSource(t)
	v := testVerifier(t, "https://hub.example", source)
	req := signed(t, source)
	req.Body = http.MaxBytesReader(nil, req.Body, 1)

	err := v.Verify(req, 7)
	require.ErrorIs(t, err, ErrInvalid)
	var limitErr *http.MaxBytesError
	require.ErrorAs(t, err, &limitErr)
	require.EqualValues(t, 1, limitErr.Limit)
}

func TestVerifierKeepsUnavailableKeysRejectedUntilSourceReturns(t *testing.T) {
	for _, mode := range []string{"missing environment", "missing file", "short key", "nonregular file"} {
		t.Run(mode, func(t *testing.T) {
			source := testSource(t)
			other := Source{KeyID: "other-key", KeyEnv: "KATA_TEST_OTHER_SIGNING_KEY"}
			t.Setenv(other.KeyEnv, strings.Repeat("b", 64))
			rejected := signed(t, source)
			if mode == "missing environment" {
				t.Setenv(source.KeyEnv, "")
			} else {
				source.KeyEnv = ""
				source.KeyFile = filepath.Join(t.TempDir(), "key")
				switch mode {
				case "short key":
					require.NoError(t, os.WriteFile(source.KeyFile, []byte("too short"), 0600))
				case "nonregular file":
					require.NoError(t, os.Mkdir(source.KeyFile, 0700))
				}
			}
			keys := []Key{{Source: source, EnrollmentID: 7}, {Source: other, EnrollmentID: 8}}
			state := filepath.Join(t.TempDir(), "replay.state")
			require.NoError(t, InitializeReplayState(state))
			v, err := NewVerifier("https://hub.example", keys, state)
			require.NoError(t, err)
			t.Cleanup(func() { _ = v.Close() })
			v.started = time.Now().Add(-Quarantine)

			require.Error(t, v.Verify(rejected, 7), "unavailable source must reject its signed requests")
			require.NoError(t, v.Verify(signed(t, other), 8), "another enrollment remains available")
			require.Error(t, ValidatePolicy("https://hub.example", keys), "explicit configuration stays strict")
			if source.KeyEnv != "" {
				t.Setenv(source.KeyEnv, strings.Repeat("a", 64))
			} else {
				if mode == "nonregular file" {
					require.NoError(t, os.Remove(source.KeyFile))
				}
				require.NoError(t, os.WriteFile(source.KeyFile, []byte(strings.Repeat("a", 64)), 0600))
			}
			require.NoError(t, v.Verify(signed(t, source), 7), "restoring the source requires no verifier restart")
		})
	}
}

func TestVerifierStartsAfterRetiredSourceRemoval(t *testing.T) {
	source := testSource(t)
	retiredRequest := signed(t, source)
	retired := Key{Source: source, EnrollmentID: 7, NotAfter: time.Now().Add(-time.Minute).Unix()}
	t.Setenv(source.KeyEnv, "")
	state := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, InitializeReplayState(state))
	v, err := NewVerifier("https://hub.example", []Key{retired}, state)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	v.started = time.Now().Add(-Quarantine)
	require.ErrorIs(t, v.Verify(retiredRequest, 7), ErrKeyRejected)
	t.Setenv(source.KeyEnv, strings.Repeat("a", 64))
	require.ErrorIs(t, v.Verify(signed(t, source), 7), ErrKeyRejected)
}

func TestVerifierRejectsDuplicateAvailableSecrets(t *testing.T) {
	source := testSource(t)
	other := source
	other.KeyID = "other-key"
	state := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, InitializeReplayState(state))
	_, err := NewVerifier("https://hub.example", []Key{
		{Source: source, EnrollmentID: 7}, {Source: other, EnrollmentID: 8},
	}, state)
	require.ErrorIs(t, err, ErrKey)
}

func signedWithoutKey(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "https://hub.example/api/v1/projects/1/federation/metadata", nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer enrollment")
	r.Header.Set("Content-Type", "application/json")
	return r
}

// Contract: client signing pins the configured origin and never follows redirects.
func TestClientRedirectAndOriginPin(t *testing.T) {
	s := testSource(t)
	var calls int
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.Header.Get("Signature"))
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer hub.Close()
	c := hub.Client()
	require.NoError(t, ConfigureClient(c, hub.URL, "enrollment", s))
	r, err := c.Get(hub.URL)
	require.NoError(t, err)
	_ = r.Body.Close()
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	_, err = c.Get(target.URL)
	require.Error(t, err)
	require.Zero(t, calls)
}

func TestDurableReplayWatermarkAndRollback(t *testing.T) {
	s := testSource(t)
	p := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, InitializeReplayState(p))
	v, err := NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, p)
	require.NoError(t, err)
	v.started = time.Now().Add(-Quarantine)
	require.NoError(t, v.Verify(signed(t, s), 7))
	_, err = NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, p)
	require.Error(t, err, "one process lock protects all verifier instances")
	watermark, err := os.ReadFile(p) //nolint:gosec // G304: replay evidence in t.TempDir.
	require.NoError(t, err)
	require.NotEqual(t, "0\n", string(watermark))
	require.NoError(t, v.Close())
	restarted, err := NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.started = time.Now().Add(-Quarantine)
	require.ErrorIs(t, restarted.Verify(signed(t, s), 7), ErrWarming, "persisted expiry also gates startup")
	require.Error(t, InitializeReplayState(p), "never reset retained replay evidence")
}

func TestConcurrentClockSamplingDoesNotFenceHealthyTraffic(t *testing.T) {
	s := testSource(t)
	v := testVerifier(t, "https://hub.example", s)
	firstRequest := signed(t, s)
	secondRequest := signed(t, s)

	firstSample := time.Now()
	secondSample := firstSample.Add(time.Millisecond)
	firstSampled := make(chan struct{})
	resumeFirst := make(chan struct{})
	var samples atomic.Int32
	v.now = func() time.Time {
		if samples.Add(1) == 1 {
			close(firstSampled)
			<-resumeFirst
			return firstSample
		}
		return secondSample
	}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- v.Verify(firstRequest, 7)
	}()
	<-firstSampled
	secondErr := v.Verify(secondRequest, 7)
	close(resumeFirst)
	firstErr := <-firstResult

	require.NoError(t, secondErr)
	require.NoError(t, firstErr)
	require.Len(t, v.seen, 2)
}

func TestBodyVerificationRespectsUploadBudgetAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    error
	}{
		{name: "full request budget", elapsed: 60 * time.Second},
		{name: "expired signature", elapsed: 91 * time.Second, want: ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSource(t)
			v := testVerifier(t, "https://hub.example", s)
			r := signed(t, s)
			now := time.Now()
			v.now = func() time.Time { return now }
			r.Body = &advancingBody{
				Reader: strings.NewReader(`{"events":[]}`),
				advance: func() {
					v.now = func() time.Time { return now.Add(tc.elapsed) }
				},
			}
			require.ErrorIs(t, v.Verify(r, 7), tc.want)
		})
	}
}

type advancingBody struct {
	*strings.Reader
	advance func()
}

func (b *advancingBody) Read(p []byte) (int, error) { b.advance(); return b.Reader.Read(p) }
func (*advancingBody) Close() error                 { return nil }

// Property: malformed signature input never admits or panics, regardless of bytes.
func FuzzSignatureInput(f *testing.F) {
	f.Add("sig1=(\"@method\");created=0")
	f.Add("sig1=()")
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > MaxHeaderBytes {
			t.Skip()
		}
		r := signedWithoutKey(t)
		r.Header.Set("Signature-Input", input)
		r.Header.Set("Signature", "sig1=:AA==:")
		r.Header.Set("Content-Digest", "sha-256=:AA==:")
		v := &Verifier{now: time.Now, started: time.Now().Add(-Quarantine), keys: map[string]Key{}, seen: map[string]time.Time{}, capacity: 1}
		require.Error(t, v.Verify(r, 7))
	})
}

// Contract: the wire MAC is RFC 9421 over the exact canonical covered fields,
// and RFC 9530 includes the digest of an empty body.
func TestWireMACAgainstIndependentReference(t *testing.T) {
	s := testSource(t)
	r := signedWithoutKey(t)
	require.NoError(t, Sign(r, s))
	digest := sha256.Sum256(nil)
	require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":", r.Header.Get("Content-Digest"))
	params := strings.TrimPrefix(r.Header.Get("Signature-Input"), "sig1=")
	canonical := `"@method": GET` + "\n" + `"@target-uri": ` + r.URL.String() + "\n" + `"content-digest": ` + r.Header.Get("Content-Digest") + "\n" + `"content-type": application/json` + "\n" + `"authorization": Bearer enrollment` + "\n" + `"@signature-params": ` + params
	mac := hmac.New(sha256.New, []byte(strings.Repeat("a", 64)))
	_, err := mac.Write([]byte(canonical))
	require.NoError(t, err)
	require.Equal(t, "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":", r.Header.Get("Signature"))
}

func TestFreshnessRotationAndBodyCancellation(t *testing.T) {
	for _, mode := range []string{"expired", "future", "rotation", "removed", "cancelled", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			s := testSource(t)
			v := testVerifier(t, "https://hub.example", s)
			r := signed(t, s)
			switch mode {
			case "expired":
				v.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			case "future":
				v.now = func() time.Time { return time.Now().Add(-10 * time.Second) }
				v.last = time.Now().Add(-time.Minute)
				v.started = time.Now().Add(-Quarantine - time.Minute)
			case "rotation":
				k := v.keys[s.KeyID]
				k.NotAfter = time.Now().Add(time.Second).Unix()
				v.keys[s.KeyID] = k
				r.Body = &advancingBody{Reader: strings.NewReader(`{"events":[]}`), advance: func() { v.now = func() time.Time { return time.Now().Add(2 * time.Second) } }}
			case "removed":
				r.Body = &advancingBody{Reader: strings.NewReader(`{"events":[]}`), advance: func() { t.Setenv(s.KeyEnv, "") }}
			case "cancelled":
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r = r.WithContext(ctx)
				r.Body = &advancingBody{Reader: strings.NewReader(`{"events":[]}`), advance: cancel}
			case "rollback":
				v.last = time.Now().Add(time.Second)
			}
			want := ErrInvalid
			if mode == "rotation" || mode == "removed" {
				want = ErrKeyRejected
			}
			if mode == "rollback" {
				want = ErrWarming
			}
			require.ErrorIs(t, v.Verify(r, 7), want)
			require.Empty(t, v.seen)
		})
	}
}

func TestReplayStateMissingCorruptOrOversizedFailsClosed(t *testing.T) {
	s := testSource(t)
	for _, data := range []string{"", "invalid\n", strings.Repeat("0", 128), "-1\n"} {
		p := filepath.Join(t.TempDir(), "state")
		require.NoError(t, os.WriteFile(p, []byte(data), 0600))
		_, err := NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, p)
		require.ErrorIs(t, err, ErrState)
	}
	_, err := NewVerifier("https://hub.example", []Key{{Source: s, EnrollmentID: 7}}, filepath.Join(t.TempDir(), "missing"))
	require.ErrorIs(t, err, ErrState)
}

func TestConfiguredSigningRejectsBearerAsSecret(t *testing.T) {
	source := testSource(t)
	client := &http.Client{}
	require.ErrorIs(t, ConfigureClient(client, "https://hub.example", strings.Repeat("a", 64), source), ErrKey)
	require.Nil(t, client.Transport, "invalid independent key must not install a transport")
}

func TestClientClosesBodyOnSigningTransportRejection(t *testing.T) {
	for _, mode := range []string{"origin", "authority", "removed key"} {
		t.Run(mode, func(t *testing.T) {
			source := testSource(t)
			calls := 0
			hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusOK)
			}))
			defer hub.Close()
			client := hub.Client()
			require.NoError(t, ConfigureClient(client, hub.URL, "enrollment", source))
			body, err := os.CreateTemp(t.TempDir(), "request-body")
			require.NoError(t, err)
			t.Cleanup(func() { _ = body.Close() })
			req, err := http.NewRequest(http.MethodPost, hub.URL, body)
			require.NoError(t, err)
			want := ErrInvalid
			switch mode {
			case "origin":
				req.URL.Scheme = "http"
			case "authority":
				req.Host = "other.example"
			case "removed key":
				t.Setenv(source.KeyEnv, "")
				want = ErrKeyRejected
			}
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorIs(t, err, want)
			require.Zero(t, calls)
			_, err = body.Stat()
			require.ErrorIs(t, err, os.ErrClosed)
		})
	}
}

func TestExternalBasesUseNativeCanonicalAuthority(t *testing.T) {
	source := testSource(t)
	source.HubURL = "https://HUB.EXAMPLE:443/federation"
	client := &http.Client{}
	require.NoError(t, ConfigureClient(client, "https://hub.example/federation", "enrollment", source))
	base, err := externalBase(source.HubURL)
	require.NoError(t, err)
	require.Equal(t, "https://hub.example/federation", base.String())
}

// Contract: an HTTP transport retry cannot resend an already admitted nonce.
func TestTransportCannotReplaySignatureAfterLostResponse(t *testing.T) {
	source := testSource(t)
	var retryInputs []string
	var mu sync.Mutex
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		retryInputs = append(retryInputs, r.Header.Get("Signature-Input"))
		first := len(retryInputs) == 1
		mu.Unlock()
		if first {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()
	client := hub.Client()
	require.NoError(t, ConfigureClient(client, hub.URL, "enrollment", source))
	resp, err := client.Get(hub.URL + "/warm")
	require.NoError(t, err)
	_ = resp.Body.Close()
	resp, err = client.Get(hub.URL + "/retry")
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "lost response returns to the native retry loop")
	mu.Lock()
	require.Len(t, retryInputs, 1)
	mu.Unlock()
	resp, err = client.Get(hub.URL + "/retry")
	require.NoError(t, err)
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, retryInputs, 2)
	require.NotEqual(t, retryInputs[0], retryInputs[1], "native retry generates a fresh nonce")
}
