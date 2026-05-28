package db

import "time"

// eventHLCTimestamp is a hybrid logical clock value split into
// SQLite-sortable parts.
type eventHLCTimestamp struct {
	PhysicalMS int64
	Counter    int64
}

// nextEventHLCValue returns the next local timestamp after last, using now when wall time
// has advanced and the logical counter when it has not.
func nextEventHLCValue(last eventHLCTimestamp, now time.Time) eventHLCTimestamp {
	n := now.UTC().UnixMilli()
	if n > last.PhysicalMS {
		return eventHLCTimestamp{PhysicalMS: n, Counter: 0}
	}
	return eventHLCTimestamp{PhysicalMS: last.PhysicalMS, Counter: last.Counter + 1}
}

// advanceEventHLC returns a local timestamp that is after both local and incoming.
func advanceEventHLC(local, incoming eventHLCTimestamp, now time.Time) eventHLCTimestamp {
	n := now.UTC().UnixMilli()
	maxPhysical := max(n, max(local.PhysicalMS, incoming.PhysicalMS))
	switch {
	case maxPhysical == local.PhysicalMS && maxPhysical == incoming.PhysicalMS:
		return eventHLCTimestamp{PhysicalMS: maxPhysical, Counter: max(local.Counter, incoming.Counter) + 1}
	case maxPhysical == local.PhysicalMS:
		return eventHLCTimestamp{PhysicalMS: maxPhysical, Counter: local.Counter + 1}
	case maxPhysical == incoming.PhysicalMS:
		return eventHLCTimestamp{PhysicalMS: maxPhysical, Counter: incoming.Counter + 1}
	default:
		return eventHLCTimestamp{PhysicalMS: maxPhysical, Counter: 0}
	}
}
