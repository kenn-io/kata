// Package federationsigning implements Kata's bounded RFC 9421 request profile.
package federationsigning

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dunglas/httpsfv"
	"github.com/gofrs/flock"
	"github.com/yaronf/httpsign"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kit/atomicfile"
)

// Protocol and admission limits define the supported signing profile.
const (
	Lifetime  = 30 * time.Second
	ClockSkew = 5 * time.Second
	// Quarantine includes one second for integer timestamps at the boundary.
	Quarantine     = Lifetime + ClockSkew + time.Second
	MaxBodyBytes   = 64 << 20
	MaxHeaderBytes = 16 << 10
)

// Admission errors never include secret or body contents.
var (
	ErrInvalid     = errors.New("invalid federation request signature")
	ErrKey         = errors.New("federation signing key unavailable")
	ErrKeyRejected = errors.New("federation signing key rejected")
	ErrWarming     = errors.New("federation signing replay protection warming up")
	ErrState       = errors.New("federation replay state unavailable")
	ErrCapacity    = errors.New("federation signing replay capacity exhausted")
	components     = []string{"@method", "@target-uri", "content-digest", "content-type", "authorization"}
)

// Source references a separate secret; it never contains the secret itself.
// HubURL pins spoke credentials to the exact configured base, including prefix.
type Source struct {
	KeyID   string `toml:"key_id"`
	KeyFile string `toml:"key_file,omitempty"`
	KeyEnv  string `toml:"key_env,omitempty"`
	HubURL  string `toml:"hub_url,omitempty"`
}

// Validate requires a public key ID and exactly one secret reference.
func (s Source) Validate() error {
	if s.KeyID == "" || len(s.KeyID) > 128 || strings.ContainsAny(s.KeyID, "\"\\\r\n\t ") ||
		(s.KeyFile == "") == (s.KeyEnv == "") {
		return ErrKey
	}
	return nil
}

// Load rereads sources on every request so source removal revokes keys.
func (s Source) Load() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var b []byte
	if s.KeyEnv != "" {
		value := os.Getenv(s.KeyEnv)
		if strings.TrimSpace(value) == "" {
			return nil, ErrKeyRejected
		}
		b = []byte(value)
	} else {
		f, err := os.Open(s.KeyFile) //nolint:gosec // G304: operator-configured secret source.
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrKeyRejected
			}
			return nil, ErrKey
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return nil, ErrKey
		}
		b, err = io.ReadAll(io.LimitReader(f, 4097))
		if err != nil {
			return nil, ErrKey
		}
	}
	b = bytes.TrimSpace(b)
	if len(b) < 64 || len(b) > 4096 {
		return nil, ErrKey
	}
	return b, nil
}

// Key binds signing authority to one native enrollment, never an actor override.
// Retiring keys must have a hard deadline; the last deadline ends overlap.
type Key struct {
	Source       `toml:",inline"`
	EnrollmentID int64 `toml:"enrollment_id"`
	NotAfter     int64 `toml:"not_after,omitempty"`
}

// Verifier owns one bounded admission cache and its durable restart barrier.
type Verifier struct {
	statePath      string
	lock           *flock.Flock
	watermark      int64
	startupBarrier int64
	last           time.Time
	failed         bool
	closed         bool
	base           *url.URL
	keys           map[string]Key
	mu             sync.Mutex
	seen           map[string]time.Time
	capacity       int
	started        time.Time
	now            func() time.Time
}

func externalBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !unambiguous(u) {
		return nil, ErrInvalid
	}
	canonical, err := httpurl.CanonicalHTTPBaseURL(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	return url.Parse(canonical)
}

// NewVerifier starts quarantined and locks the durable admission-expiry watermark.
// Each concurrently serving replica must use distinct enrollment signing keys.
func NewVerifier(base string, keys []Key, statePath string) (*Verifier, error) {
	if err := ValidatePolicy(base, keys); err != nil {
		return nil, err
	}
	u, err := externalBase(base)
	if err != nil {
		return nil, err
	}
	v := &Verifier{base: u, keys: make(map[string]Key), seen: make(map[string]time.Time), capacity: 8192, started: time.Now(), now: time.Now}
	for _, k := range keys {
		if k.EnrollmentID <= 0 {
			return nil, ErrKey
		}
		if _, ok := v.keys[k.KeyID]; ok {
			return nil, ErrKey
		}
		if _, err := k.Load(); err != nil {
			return nil, err
		}
		v.keys[k.KeyID] = k
	}

	if statePath == "" {
		return nil, ErrState
	}
	lock := flock.New(statePath+".lock", flock.SetPermissions(0600))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, ErrState
	}
	f, err := os.Open(statePath) //nolint:gosec // G304: operator-configured replay state.
	if err != nil {
		_ = lock.Unlock()
		return nil, ErrState
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	info, statErr := f.Stat()
	_ = f.Close()
	if err != nil || len(data) > 64 {
		_ = lock.Unlock()
		return nil, ErrState
	}
	watermark, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || watermark < 0 || string(data) != strconv.FormatInt(watermark, 10)+"\n" || statErr != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		_ = lock.Unlock()
		return nil, ErrState
	}
	v.statePath, v.lock, v.watermark, v.startupBarrier, v.last = statePath, lock, watermark, watermark, v.started
	return v, nil
}

func unambiguous(u *url.URL) bool {
	p := u.Path
	// Federation identifiers are ASCII. Reject encoded path aliases rather than
	// letting a proxy, ServeMux and the verifier disagree about routing.
	return u.RawPath == "" && !strings.ContainsAny(p, "%\\\x00\r\n") &&
		!strings.Contains(p, "//") && (p == "" || path.Clean(p) == p) && !u.ForceQuery
}

func singleHeaders(r *http.Request) bool {
	if len(r.Trailer) != 0 || r.Header.Get("Trailer") != "" || r.Header.Get("Content-Encoding") != "" {
		return false
	}
	n := 0
	for k, values := range r.Header {
		for _, value := range values {
			n += len(k) + len(value) + 4
		}
	}
	if n > MaxHeaderBytes {
		return false
	}
	for _, name := range []string{"Authorization", "Content-Type", "Content-Digest", "Signature-Input", "Signature"} {
		if len(r.Header.Values(name)) != 1 || r.Header.Get(name) == "" {
			return false
		}
	}
	return true
}

func profile(r *http.Request) bool {
	input, err := httpsfv.UnmarshalDictionary(r.Header.Values("Signature-Input"))
	if err != nil || len(input.Names()) != 1 {
		return false
	}
	member, _ := input.Get("sig1")
	list, ok := member.(httpsfv.InnerList)
	if !ok || len(list.Items) != len(components) || len(list.Params.Names()) != 5 {
		return false
	}
	for i, item := range list.Items {
		if item.Value != components[i] || len(item.Params.Names()) != 0 {
			return false
		}
	}
	// Reject duplicate dictionary members and parameters (structured-field parsers
	// otherwise retain the last occurrence). Our profile uses canonical SF syntax.
	canonical, err := httpsfv.Marshal(input)
	if err != nil || canonical != r.Header.Get("Signature-Input") {
		return false
	}
	sig, err := httpsfv.UnmarshalDictionary(r.Header.Values("Signature"))
	if err != nil || len(sig.Names()) != 1 {
		return false
	}
	sigCanonical, err := httpsfv.Marshal(sig)
	if err != nil || sigCanonical != r.Header.Get("Signature") {
		return false
	}
	return true
}

// Verify checks the MAC before reading a bounded body, then admits the nonce
// atomically. The caller must first authenticate the live native enrollment.
func (v *Verifier) Verify(r *http.Request, enrollmentID int64) error {
	if !singleHeaders(r) || !unambiguous(r.URL) || !profile(r) {
		return ErrInvalid
	}
	now := v.now()
	v.mu.Lock()
	clockNow := v.now()
	unavailable := v.closed || v.failed
	warming := clockNow.Sub(v.started) < Quarantine || clockNow.Unix() <= v.startupBarrier || clockNow.UnixNano() < v.last.UnixNano()
	if clockNow.UnixNano() > v.last.UnixNano() {
		v.last = clockNow
	}
	v.mu.Unlock()
	if unavailable {
		return ErrState
	}
	if warming {
		return ErrWarming
	}
	d, err := httpsign.RequestDetails("sig1", r)
	if err != nil || d.KeyID == nil || d.Alg != "hmac-sha256" || d.Created == nil || d.Expires == nil || d.Nonce == nil ||
		len(*d.Nonce) != 32 || d.Created.After(now.Add(ClockSkew)) || now.After(*d.Expires) ||
		!d.Expires.After(*d.Created) || d.Expires.Sub(*d.Created) > Lifetime || now.Sub(*d.Created) > Lifetime+ClockSkew {
		return ErrInvalid
	}
	if _, err := base64.RawURLEncoding.DecodeString(*d.Nonce); err != nil {
		return ErrInvalid
	}
	k, ok := v.keys[*d.KeyID]
	if !ok || k.EnrollmentID != enrollmentID || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return ErrKeyRejected
	}
	secret, err := k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))) == 1 {
		return ErrKey
	}
	cloned := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host, u.Path = v.base.Scheme, v.base.Host, v.base.Path+r.URL.Path
	cloned.URL, cloned.Host = &u, v.base.Host
	vc := httpsign.NewVerifyConfig().SetVerifyCreated(false).SetRejectExpired(false).SetKeyID(k.KeyID)
	verifier, err := httpsign.NewHMACSHA256Verifier(secret, vc, httpsign.Headers(components...))
	if err != nil || httpsign.VerifyRequest("sig1", *verifier, cloned) != nil {
		return ErrInvalid
	}
	if r.ContentLength > MaxBodyBytes {
		return ErrInvalid
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		_ = r.Body.Close()
		if err != nil || len(body) > MaxBodyBytes {
			return ErrInvalid
		}
	}
	digest := sha256.Sum256(body)
	expected := "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
	if subtle.ConstantTimeCompare([]byte(expected), []byte(r.Header.Get("Content-Digest"))) != 1 {
		return ErrInvalid
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	if r.Context().Err() != nil {
		return ErrInvalid
	}
	now = v.now()
	if !fresh(d, now) {
		return ErrInvalid
	}
	current, err := k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, current) != 1 || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return ErrKeyRejected
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now = v.now()
	if v.closed || v.failed {
		return ErrState
	}
	if now.UnixNano() < v.last.UnixNano() {
		return ErrWarming
	}
	if r.Context().Err() != nil || !fresh(d, now) {
		return ErrInvalid
	}
	if k.NotAfter != 0 && now.Unix() >= k.NotAfter {
		return ErrKeyRejected
	}
	v.last = now
	for nonce, expiry := range v.seen {
		if now.After(expiry) {
			delete(v.seen, nonce)
		}
	}
	nonce := k.KeyID + ":" + *d.Nonce
	if _, exists := v.seen[nonce]; exists {
		return ErrInvalid
	}
	if len(v.seen) >= v.capacity {
		return ErrCapacity
	}
	if d.Expires.Unix() > v.watermark {
		if err := atomicfile.WriteFile(v.statePath, []byte(strconv.FormatInt(d.Expires.Unix(), 10)+"\n"), atomicfile.WithPrivate()); err != nil {
			v.failed = true
			return ErrState
		}
		v.watermark = d.Expires.Unix()
	}
	now = v.now()
	if now.UnixNano() < v.last.UnixNano() {
		return ErrWarming
	}
	if r.Context().Err() != nil || !fresh(d, now) {
		return ErrInvalid
	}
	current, err = k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, current) != 1 || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return ErrKeyRejected
	}
	v.last = now
	v.seen[nonce] = *d.Expires
	return nil
}

// Sign covers exact bytes and a fresh random nonce on each attempt.
func Sign(r *http.Request, source Source) error {
	if !unambiguous(r.URL) {
		return ErrInvalid
	}
	r.Header.Set("Content-Type", "application/json")
	key, err := source.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(key, []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))) == 1 {
		return ErrKey
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrKey
	}
	sc := httpsign.NewSignConfig().SetKeyID(source.KeyID).SetExpiresAfter(int64(Lifetime / time.Second)).SetNonce(base64.RawURLEncoding.EncodeToString(nonce[:]))
	signer, err := httpsign.NewHMACSHA256Signer(key, sc, httpsign.Headers(components...))
	if err != nil {
		return ErrKey
	}
	digest, err := httpsign.GenerateContentDigestHeader(&r.Body, []string{httpsign.DigestSha256}, httpsign.NewDigestOptions().SetMaxBodySize(MaxBodyBytes))
	if err != nil {
		return ErrInvalid
	}
	r.Header.Set("Content-Digest", digest)
	input, sig, err := httpsign.SignRequest("sig1", *signer, r)
	if err != nil {
		return ErrInvalid
	}
	r.Header.Set("Signature-Input", input)
	r.Header.Set("Signature", sig)
	return nil
}

type transport struct {
	next   http.RoundTripper
	base   *url.URL
	token  string
	source Source
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	origin, err := httpurl.CanonicalHTTPOrigin(r.URL.String())
	baseOrigin := (&url.URL{Scheme: t.base.Scheme, Host: t.base.Host}).String()
	if err != nil || origin != baseOrigin || r.URL.Fragment != "" ||
		(r.URL.Path != t.base.Path && !strings.HasPrefix(r.URL.Path, t.base.Path+"/")) {
		return nil, ErrInvalid
	}
	if r.Host != "" {
		authority, err := httpurl.CanonicalHTTPOrigin(t.base.Scheme + "://" + r.Host)
		if err != nil || authority != baseOrigin {
			return nil, ErrInvalid
		}
	}
	cloned := r.Clone(r.Context())
	cloned.URL.Scheme, cloned.URL.Host = t.base.Scheme, t.base.Host
	cloned.Host = t.base.Host
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	if err := Sign(cloned, t.source); err != nil {
		return nil, err
	}
	// The underlying Transport must not transparently resend a nonce after
	// a response is lost. Native retries reenter this signer with a fresh nonce.
	cloned.GetBody = nil
	if cloned.Body == nil || cloned.Body == http.NoBody {
		cloned.Body = io.NopCloser(bytes.NewReader(nil))
	}
	return t.next.RoundTrip(cloned)
}

// ConfigureClient preserves the native bearer transport and TLS validation.
func ConfigureClient(c *http.Client, base, token string, source Source) error {
	u, err := externalBase(base)
	if err != nil || token == "" {
		return ErrInvalid
	}
	if source.HubURL != "" {
		pinned, err := externalBase(source.HubURL)
		if err != nil || pinned.String() != u.String() {
			return ErrInvalid
		}
	}
	secret, err := source.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, []byte(token)) == 1 {
		return ErrKey
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = transport{next: next, base: u, token: token, source: source}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return nil
}

// InitializeReplayState creates new state only; it never resets prior evidence.
// After losing state, rotate every accepted key before initializing a replacement.
func InitializeReplayState(path string) error {
	return atomicfile.WriteNew(path, []byte("0\n"), atomicfile.WithPrivate())
}

// Close releases replay ownership and permanently fences this verifier.
func (v *Verifier) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	if v.lock != nil {
		return v.lock.Unlock()
	}
	return nil
}

func fresh(d *httpsign.MessageDetails, now time.Time) bool {
	return d.Created != nil && d.Expires != nil && !d.Created.After(now.Add(ClockSkew)) && !now.After(*d.Expires) && d.Expires.After(*d.Created) && d.Expires.Sub(*d.Created) <= Lifetime && now.Sub(*d.Created) <= Lifetime+ClockSkew
}

// ValidatePolicyShape checks the public policy and source references without
// loading signing secrets or acquiring replay state.
func ValidatePolicyShape(base string, keys []Key) error {
	if _, err := externalBase(base); err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrKey
	}
	ids := map[string]bool{}
	for _, k := range keys {
		if k.EnrollmentID <= 0 || ids[k.KeyID] {
			return ErrKey
		}
		ids[k.KeyID] = true
		if err := k.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePolicy checks configured sources without acquiring or modifying replay state.
func ValidatePolicy(base string, keys []Key) error {
	if err := ValidatePolicyShape(base, keys); err != nil {
		return err
	}
	secrets := map[string]bool{}
	for _, k := range keys {
		key, err := k.Load()
		if err != nil {
			return err
		}
		if secrets[string(key)] {
			return ErrKey
		}
		secrets[string(key)] = true
	}
	return nil
}
