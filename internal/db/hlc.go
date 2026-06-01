package db

import "time"

// EventHLCTimestamp is a hybrid logical clock value split into
// SQLite-sortable parts. Both backends share the same HLC arithmetic.
type EventHLCTimestamp struct {
	PhysicalMS int64
	Counter    int64
}

// NextEventHLCValue returns the next local timestamp after last, using now
// when wall time has advanced and the logical counter when it has not.
func NextEventHLCValue(last EventHLCTimestamp, now time.Time) EventHLCTimestamp {
	n := now.UTC().UnixMilli()
	if n > last.PhysicalMS {
		return EventHLCTimestamp{PhysicalMS: n, Counter: 0}
	}
	return EventHLCTimestamp{PhysicalMS: last.PhysicalMS, Counter: last.Counter + 1}
}

// AdvanceEventHLC returns a local timestamp that is after both local and incoming.
func AdvanceEventHLC(local, incoming EventHLCTimestamp, now time.Time) EventHLCTimestamp {
	n := now.UTC().UnixMilli()
	maxPhysical := max(n, max(local.PhysicalMS, incoming.PhysicalMS))
	switch {
	case maxPhysical == local.PhysicalMS && maxPhysical == incoming.PhysicalMS:
		return EventHLCTimestamp{PhysicalMS: maxPhysical, Counter: max(local.Counter, incoming.Counter) + 1}
	case maxPhysical == local.PhysicalMS:
		return EventHLCTimestamp{PhysicalMS: maxPhysical, Counter: local.Counter + 1}
	case maxPhysical == incoming.PhysicalMS:
		return EventHLCTimestamp{PhysicalMS: maxPhysical, Counter: incoming.Counter + 1}
	default:
		return EventHLCTimestamp{PhysicalMS: maxPhysical, Counter: 0}
	}
}
