package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// The boundary applies to event and artifact reads of the superseded epoch and
// to the activation that leaves it, never to receipts, a different epoch or an
// unrelated transition.
func TestRelayResetSupersededDeliveriesBoundaryFor(t *testing.T) {
	record := db.RelayResetSupersededDeliveries{Epoch: 4, NextEpoch: 5, ThroughID: 120}
	cases := []struct {
		name             string
		stream           string
		epoch, requested int64
		want             int64
	}{
		{name: "old epoch event read", stream: db.RelayStreamEvent, epoch: 4, requested: 4, want: 120},
		{name: "old epoch artifact read", stream: db.RelayStreamArtifact, epoch: 4, requested: 4, want: 120},
		{name: "receipts stay owed", stream: db.RelayStreamReceipt, epoch: 4, requested: 4, want: 0},
		{name: "activation into next epoch", stream: db.RelayStreamEvent, epoch: 4, requested: 5, want: 120},
		{name: "already activated", stream: db.RelayStreamEvent, epoch: 5, requested: 5, want: 0},
		{name: "stale record from an earlier transition", stream: db.RelayStreamEvent, epoch: 6, requested: 7, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, record.BoundaryFor(tc.stream, tc.epoch, tc.requested))
		})
	}
	require.Zero(t, db.RelayResetSupersededDeliveries{}.BoundaryFor(db.RelayStreamEvent, 0, 0), "a missing record never hides rows")
}
