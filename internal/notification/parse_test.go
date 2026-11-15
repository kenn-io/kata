package notification_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
)

func TestParseValuePreservesInboxCompatibility(t *testing.T) {
	for _, handle := range []any{"worker", "@invalid", 42, nil} {
		value, ok := notification.ParseValue(map[string]any{"from": " sender ", "message": " work ", "teammate": handle})
		require.True(t, ok)
		assert.Equal(t, " sender ", value.From)
		assert.Equal(t, " work ", value.Message)
		if handle == "worker" {
			assert.Equal(t, "worker", value.Teammate)
		} else {
			assert.Empty(t, value.Teammate)
		}
	}
	for _, value := range []any{nil, "string", map[string]any{"from": "sender", "message": " "}, map[string]any{"from": 42, "message": "work"}} {
		_, ok := notification.ParseValue(value)
		assert.False(t, ok)
	}
}
