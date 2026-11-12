// Package signingpolicy holds federation request-signing configuration:
// secret sources, enrollment keys, and policy validation. It is separate from
// federationsigning so configuration readers and clients do not link the
// RFC 9421 signer and verifier.
package signingpolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/kata/internal/httpurl"
)

// Policy validation and request admission share these errors. They never
// include secret or body contents.
var (
	ErrInvalid      = errors.New("invalid federation request signature")
	ErrKey          = errors.New("federation signing key unavailable")
	ErrKeyRejected  = errors.New("federation signing key rejected")
	ErrSharedSecret = errors.New("federation signing keys share one secret")
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
		info, err := os.Stat(s.KeyFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrKeyRejected
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, ErrKey
		}
		f, err := os.Open(s.KeyFile) //nolint:gosec // G304: operator-configured secret source.
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrKeyRejected
			}
			return nil, ErrKey
		}
		defer func() { _ = f.Close() }()
		info, err = f.Stat()
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

// ExternalBase parses the HTTPS base URL a signing policy is pinned to.
func ExternalBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !Unambiguous(u) {
		return nil, ErrInvalid
	}
	canonical, err := httpurl.CanonicalHTTPBaseURL(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	return url.Parse(canonical)
}

// Unambiguous reports whether u's path has exactly one routing interpretation.
func Unambiguous(u *url.URL) bool {
	p := u.Path
	// Federation identifiers are ASCII. Reject encoded path aliases rather than
	// letting a proxy, ServeMux and the verifier disagree about routing.
	return u.RawPath == "" && !strings.ContainsAny(p, "%\\\x00\r\n") &&
		!strings.Contains(p, "//") && (p == "" || path.Clean(p) == p) && !u.ForceQuery
}

// ValidatePolicyShape checks the public policy and source references without
// loading signing secrets or acquiring replay state.
func ValidatePolicyShape(base string, keys []Key) error {
	if _, err := ExternalBase(base); err != nil {
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
	for _, k := range keys {
		if _, err := k.Load(); err != nil {
			return err
		}
	}
	return CheckDistinctSecrets(keys, time.Now())
}

// CheckDistinctSecrets rejects equal available secrets among unexpired keys.
// Unavailable sources remain non-fatal at daemon startup.
func CheckDistinctSecrets(keys []Key, now time.Time) error {
	secrets := map[string]string{}
	for _, k := range keys {
		if k.NotAfter != 0 && now.Unix() >= k.NotAfter {
			continue
		}
		secret, err := k.Load()
		if err != nil {
			continue
		}
		if first, ok := secrets[string(secret)]; ok {
			return fmt.Errorf("%w: keys %q and %q; give each key its own key_file or key_env secret", ErrSharedSecret, first, k.KeyID)
		}
		secrets[string(secret)] = k.KeyID
	}
	return nil
}
