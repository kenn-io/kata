package mcpevents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/mcpevents"
)

func TestWireNameHeaderMismatchAndFallback(t *testing.T) {
	assert.Equal(t, "HeaderMismatch", mcpevents.WireName(mcpevents.CodeHeaderMismatch))
	assert.Equal(t, "Unavailable", mcpevents.WireName(12345))
}

func TestTTLMillisInvertsTTLFromWire(t *testing.T) {
	ms, noExpiry := mcpevents.TTLMS{}.Millis()
	assert.Nil(t, ms)
	assert.False(t, noExpiry)
	ms, noExpiry = mcpevents.TTLNull().Millis()
	assert.Nil(t, ms)
	assert.True(t, noExpiry)
	ms, noExpiry = mcpevents.TTLValue(90_000).Millis()
	if assert.NotNil(t, ms) {
		assert.EqualValues(t, 90_000, *ms)
	}
	assert.False(t, noExpiry)
}
