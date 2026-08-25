package pgstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatExternalObservationTimePreservesSubMillisecondPrecision(t *testing.T) {
	observedAt := time.Date(2026, 8, 20, 10, 0, 0, 123900000, time.UTC)

	assert.Equal(t, "2026-08-20T10:00:00.123900000Z", formatExternalObservationTime(observedAt))
}

func TestFormatExternalObservationTimeSortsChronologicallyAsText(t *testing.T) {
	earlier := time.Date(2026, 8, 20, 10, 0, 0, 120000000, time.UTC)
	later := time.Date(2026, 8, 20, 10, 0, 0, 123000000, time.UTC)

	assert.Less(t, formatExternalObservationTime(earlier), formatExternalObservationTime(later))
}
