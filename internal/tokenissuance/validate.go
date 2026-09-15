// Package tokenissuance validates one-time scoped credential responses before
// a client delivers their plaintext through a protected local file.
package tokenissuance

import (
	"errors"
	"strings"
	"time"
)

// Scope is the immutable grant identity a client requested and expects back.
type Scope struct {
	Kind         string
	ProjectUID   string
	RootIssueUID string
}

// Response is the security-relevant subset of a scoped token response.
type Response struct {
	ID        int64
	Actor     string
	Scope     *Scope
	CreatedAt time.Time
	ExpiresAt *time.Time
	Plaintext string
}

// Validate confirms that a daemon did not omit or widen the requested grant.
// CreatedAt and ExpiresAt are server-clock values, so their delta validates the
// requested lifetime without depending on client/server clock synchronization.
func Validate(response Response, actor string, scope Scope, duration time.Duration) error {
	if response.ID <= 0 || strings.TrimSpace(response.Plaintext) == "" {
		return errors.New("daemon returned an incomplete scoped token")
	}
	if response.Actor != actor {
		return errors.New("daemon returned a different scoped token actor")
	}
	if response.Scope == nil || *response.Scope != scope {
		return errors.New("daemon returned a missing or different token scope")
	}
	if response.ExpiresAt == nil || response.CreatedAt.IsZero() {
		return errors.New("daemon returned a missing scoped token expiration")
	}
	wantExpiry := response.CreatedAt.Add(duration)
	if delta := response.ExpiresAt.Sub(wantExpiry); delta < -2*time.Second || delta > 2*time.Second {
		return errors.New("daemon returned a different scoped token expiration")
	}
	return nil
}
