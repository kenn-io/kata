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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dunglas/httpsfv"
	"github.com/gofrs/flock"
	"github.com/yaronf/httpsign"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kit/atomicfile"
)

// Protocol and admission limits define the supported signing profile.
const (
	// Lifetime covers the 60-second request budget plus signing and transit time.
	Lifetime  = 90 * time.Second
	ClockSkew = 5 * time.Second
	// Quarantine includes one second for integer timestamps at the boundary.
	Quarantine     = Lifetime + ClockSkew + time.Second
	MaxBodyBytes   = 128 << 20
	MaxHeaderBytes = 16 << 10
)

// Admission errors never include secret or body contents.
var (
	ErrWarming  = errors.New("federation signing replay protection warming up")
	ErrState    = errors.New("federation replay state unavailable")
	ErrCapacity = errors.New("federation signing replay capacity exhausted")
	// ErrHubRejected is a hub's answer to this daemon's own request signature.
	// It is a signing configuration problem on one of the two daemons, never a
	// revoked grant, so callers keep the binding and report it as such.
	ErrHubRejected = errors.New("hub rejected the request signature; configure request signing for this enrollment")
	components     = []string{"@method", "@target-uri", "content-digest", "content-type", "authorization"}
)

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
	keys           map[string]signingpolicy.Key
	mu             sync.Mutex
	seen           map[string]time.Time
	capacity       int
	started        time.Time
	now            func() time.Time
}

// NewVerifier starts quarantined and locks the durable admission-expiry watermark.
// Each concurrently serving replica must use distinct enrollment signing keys.
// Unavailable sources reject their own requests without disabling the daemon.
func NewVerifier(base string, keys []signingpolicy.Key, statePath string) (*Verifier, error) {
	if err := signingpolicy.ValidatePolicyShape(base, keys); err != nil {
		return nil, err
	}
	u, err := signingpolicy.ExternalBase(base)
	if err != nil {
		return nil, err
	}
	v := &Verifier{
		base: u, keys: make(map[string]signingpolicy.Key), seen: make(map[string]time.Time),
		capacity: 8192, started: time.Now(), now: time.Now,
	}
	secrets := map[string]bool{}
	for _, k := range keys {
		if k.NotAfter != 0 && v.started.Unix() >= k.NotAfter {
			continue
		}
		if key, err := k.Load(); err == nil {
			if secrets[string(key)] {
				return nil, signingpolicy.ErrKey
			}
			secrets[string(key)] = true
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
	if !singleHeaders(r) || !signingpolicy.Unambiguous(r.URL) || !profile(r) {
		return signingpolicy.ErrInvalid
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
	if err != nil || d.KeyID == nil || d.Alg != "hmac-sha256" || d.Nonce == nil ||
		len(*d.Nonce) != 32 || !fresh(d, now) {
		return signingpolicy.ErrInvalid
	}
	if _, err := base64.RawURLEncoding.DecodeString(*d.Nonce); err != nil {
		return signingpolicy.ErrInvalid
	}
	k, ok := v.keys[*d.KeyID]
	if !ok || k.EnrollmentID != enrollmentID || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return signingpolicy.ErrKeyRejected
	}
	secret, err := k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))) == 1 {
		return signingpolicy.ErrKey
	}
	cloned := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host, u.Path = v.base.Scheme, v.base.Host, v.base.Path+r.URL.Path
	cloned.URL, cloned.Host = &u, v.base.Host
	vc := httpsign.NewVerifyConfig().SetVerifyCreated(false).SetRejectExpired(false).SetKeyID(k.KeyID)
	verifier, err := httpsign.NewHMACSHA256Verifier(secret, vc, httpsign.Headers(components...))
	if err != nil || httpsign.VerifyRequest("sig1", *verifier, cloned) != nil {
		return signingpolicy.ErrInvalid
	}
	if r.ContentLength > MaxBodyBytes {
		return signingpolicy.ErrInvalid
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		_ = r.Body.Close()
		if err != nil {
			return errors.Join(signingpolicy.ErrInvalid, err)
		}
		if len(body) > MaxBodyBytes {
			return signingpolicy.ErrInvalid
		}
	}
	digest := sha256.Sum256(body)
	expected := "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
	if subtle.ConstantTimeCompare([]byte(expected), []byte(r.Header.Get("Content-Digest"))) != 1 {
		return signingpolicy.ErrInvalid
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	if r.Context().Err() != nil {
		return signingpolicy.ErrInvalid
	}
	now = v.now()
	if !fresh(d, now) {
		return signingpolicy.ErrInvalid
	}
	current, err := k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, current) != 1 || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return signingpolicy.ErrKeyRejected
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
		return signingpolicy.ErrInvalid
	}
	if k.NotAfter != 0 && now.Unix() >= k.NotAfter {
		return signingpolicy.ErrKeyRejected
	}
	v.last = now
	for nonce, expiry := range v.seen {
		if now.After(expiry) {
			delete(v.seen, nonce)
		}
	}
	nonce := k.KeyID + ":" + *d.Nonce
	if _, exists := v.seen[nonce]; exists {
		return signingpolicy.ErrInvalid
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
		return signingpolicy.ErrInvalid
	}
	current, err = k.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, current) != 1 || (k.NotAfter != 0 && now.Unix() >= k.NotAfter) {
		return signingpolicy.ErrKeyRejected
	}
	v.last = now
	v.seen[nonce] = *d.Expires
	return nil
}

// Sign covers exact bytes and a fresh random nonce on each attempt.
func Sign(r *http.Request, source signingpolicy.Source) error {
	if !signingpolicy.Unambiguous(r.URL) {
		return signingpolicy.ErrInvalid
	}
	r.Header.Set("Content-Type", "application/json")
	key, err := source.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(key, []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))) == 1 {
		return signingpolicy.ErrKey
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return signingpolicy.ErrKey
	}
	sc := httpsign.NewSignConfig().SetKeyID(source.KeyID).SetExpiresAfter(int64(Lifetime / time.Second)).SetNonce(base64.RawURLEncoding.EncodeToString(nonce[:]))
	signer, err := httpsign.NewHMACSHA256Signer(key, sc, httpsign.Headers(components...))
	if err != nil {
		return signingpolicy.ErrKey
	}
	digest, err := httpsign.GenerateContentDigestHeader(&r.Body, []string{httpsign.DigestSha256}, httpsign.NewDigestOptions().SetMaxBodySize(MaxBodyBytes))
	if err != nil {
		return signingpolicy.ErrInvalid
	}
	r.Header.Set("Content-Digest", digest)
	input, sig, err := httpsign.SignRequest("sig1", *signer, r)
	if err != nil {
		return signingpolicy.ErrInvalid
	}
	r.Header.Set("Signature-Input", input)
	r.Header.Set("Signature", sig)
	return nil
}

type transport struct {
	next   http.RoundTripper
	base   *url.URL
	token  string
	source signingpolicy.Source
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	forwarded := false
	defer func() {
		if !forwarded && r.Body != nil {
			_ = r.Body.Close()
		}
	}()
	origin, err := httpurl.CanonicalHTTPOrigin(r.URL.String())
	baseOrigin := (&url.URL{Scheme: t.base.Scheme, Host: t.base.Host}).String()
	if err != nil || origin != baseOrigin || r.URL.Fragment != "" ||
		(r.URL.Path != t.base.Path && !strings.HasPrefix(r.URL.Path, t.base.Path+"/")) {
		return nil, signingpolicy.ErrInvalid
	}
	if r.Host != "" {
		authority, err := httpurl.CanonicalHTTPOrigin(t.base.Scheme + "://" + r.Host)
		if err != nil || authority != baseOrigin {
			return nil, signingpolicy.ErrInvalid
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
	forwarded = true
	return t.next.RoundTrip(cloned)
}

// ConfigureClient preserves the native bearer transport and TLS validation.
func ConfigureClient(c *http.Client, base, token string, source signingpolicy.Source) error {
	u, err := signingpolicy.ExternalBase(base)
	if err != nil || token == "" {
		return signingpolicy.ErrInvalid
	}
	if source.HubURL != "" {
		pinned, err := signingpolicy.ExternalBase(source.HubURL)
		if err != nil || pinned.String() != u.String() {
			return signingpolicy.ErrInvalid
		}
	}
	secret, err := source.Load()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(secret, []byte(token)) == 1 {
		return signingpolicy.ErrKey
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
	return d.Created != nil && d.Expires != nil &&
		!d.Created.After(now.Add(ClockSkew)) && !now.After(*d.Expires) &&
		d.Expires.After(*d.Created) && d.Expires.Sub(*d.Created) <= Lifetime &&
		now.Sub(*d.Created) <= Lifetime+ClockSkew
}
