// Package uid wraps ULID generation and validation for kata stable IDs.
package uid

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

const encodedLen = 26

// EncodedLen is the fixed character length of a Crockford base32-encoded ULID.
const EncodedLen = encodedLen

var (
	monoMu      sync.Mutex
	monoEntropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns a fresh monotonic ULID string.
func New() (string, error) {
	monoMu.Lock()
	defer monoMu.Unlock()
	id, err := ulid.New(ulid.Timestamp(time.Now().UTC()), monoEntropy)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// FromTime returns a ULID whose timestamp is t and whose entropy is random.
// It is not deterministic and must not be used for JSONL v1->v2 fill rules.
func FromTime(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t.UTC()), rand.Reader)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// FromStableSeed returns a deterministic ULID whose timestamp is t and whose
// 80-bit entropy is SHA-256(seed)[:10].
func FromStableSeed(seed []byte, t time.Time) (string, error) {
	sum := sha256.Sum256(seed)
	id, err := ulid.New(ulid.Timestamp(t.UTC()), bytes.NewReader(sum[:10]))
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Length is the fixed character length of a kata ULID (Crockford base32).
func Length() int { return encodedLen }

// Canonical returns the uppercase spelling of a valid ULID. Opaque IDs and
// malformed values retain their exact spelling.
func Canonical(value string) string {
	if len(value) != encodedLen {
		return value
	}
	bytes := []byte(value)
	for i, char := range bytes {
		if char >= 'a' && char <= 'z' {
			bytes[i] = char - ('a' - 'A')
		}
	}
	canonical := string(bytes)
	if Valid(canonical) {
		return canonical
	}
	return value
}

// Valid reports whether s is a strict 26-character ULID.
func Valid(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil
}

// ValidPrefix reports whether s is a valid leftmost ULID prefix.
func ValidPrefix(s string) bool {
	if len(s) == 0 || len(s) > encodedLen {
		return false
	}
	if s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !validChar(s[i]) {
			return false
		}
	}
	if len(s) == encodedLen {
		return Valid(s)
	}
	return true
}

// MustTime extracts the timestamp from a valid ULID and panics on invalid
// input. It is intended for tests and invariant-checked call sites.
func MustTime(s string) time.Time {
	id, err := ulid.ParseStrict(s)
	if err != nil {
		panic(err)
	}
	return ulid.Time(id.Time()).UTC()
}

func validChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		switch c {
		case 'I', 'L', 'O', 'U':
			return false
		default:
			return true
		}
	default:
		return false
	}
}
