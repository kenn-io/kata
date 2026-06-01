package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHLCNextAdvancesCounterWhenClockDoesNotMoveForward(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	first := NextEventHLCValue(EventHLCTimestamp{}, now)
	second := NextEventHLCValue(first, now.Add(-time.Second))

	assert.Equal(t, first.PhysicalMS, second.PhysicalMS)
	assert.Equal(t, first.Counter+1, second.Counter)
}

func TestHLCAdvanceMovesPastIncomingForeignTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	local := EventHLCTimestamp{PhysicalMS: now.UnixMilli(), Counter: 2}
	incoming := EventHLCTimestamp{PhysicalMS: now.Add(time.Second).UnixMilli(), Counter: 4}

	got := AdvanceEventHLC(local, incoming, now)

	assert.Equal(t, incoming.PhysicalMS, got.PhysicalMS)
	assert.Equal(t, incoming.Counter+1, got.Counter)
}
