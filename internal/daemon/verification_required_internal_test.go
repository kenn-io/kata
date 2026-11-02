package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestVerificationRequiredBooleanOnly(t *testing.T) {
	for _, raw := range []string{`{}`, `{"verify.required":false}`, `{"verify.required":"true"}`, `{"verify.required":1}`, `{"verify.required":null}`, `{"verify.required":true}`} {
		require.Equal(t, raw == `{"verify.required":true}`, verificationRequired(db.Issue{Metadata: db.JSONBlob(raw)}), raw)
	}
}
