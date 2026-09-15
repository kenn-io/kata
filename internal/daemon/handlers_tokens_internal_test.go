package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/db"
)

func TestTokenLifecycleStateUsesServerObservationTimeAndRevocationPrecedence(t *testing.T) {
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	expiredAt := now
	revokedAt := now.Add(-time.Hour)

	assert.Equal(t, "live", tokenLifecycleState(db.APIToken{}, now))
	assert.Equal(t, "expired", tokenLifecycleState(db.APIToken{ExpiresAt: &expiredAt}, now))
	assert.Equal(t, "revoked", tokenLifecycleState(db.APIToken{
		ExpiresAt: &expiredAt, RevokedAt: &revokedAt,
	}, now))
}
