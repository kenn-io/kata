package db

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetainFederationEventsFetchesMissingEventsInOneCall(t *testing.T) {
	audit := Event{UID: "AUDIT"}
	var calls [][]string
	fetch := func(_ context.Context, ids []string) ([]Event, error) {
		calls = append(calls, append([]string(nil), ids...))
		events := make([]Event, 0, len(ids))
		for _, id := range slices.Backward(ids) {
			events = append(events, Event{UID: id})
		}
		return events, nil
	}

	ordered, events, err := RetainFederationEvents(t.Context(), []string{"A", "AUDIT", "B", "A"}, []Event{audit}, fetch)
	require.NoError(t, err)
	require.Equal(t, []string{"A", "AUDIT", "B"}, ordered)
	require.Equal(t, []Event{{UID: "A"}, audit, {UID: "B"}}, events, "events keep admission order")
	require.Equal(t, [][]string{{"A", "B"}}, calls, "only events not produced in the transaction are fetched, together")
}

func TestRetainFederationEventsSkipsFetchWhenEverythingWasProduced(t *testing.T) {
	fetch := func(context.Context, []string) ([]Event, error) {
		t.Fatal("fetch must not run")
		return nil, nil
	}
	ordered, events, err := RetainFederationEvents(t.Context(), nil, []Event{{UID: "AUDIT"}}, fetch)
	require.NoError(t, err)
	require.Equal(t, []string{"AUDIT"}, ordered)
	require.Equal(t, []Event{{UID: "AUDIT"}}, events)
}

func TestRetainFederationEventsReportsUnfetchedEvent(t *testing.T) {
	fetch := func(context.Context, []string) ([]Event, error) { return nil, nil }
	_, _, err := RetainFederationEvents(t.Context(), []string{"A"}, nil, fetch)
	require.ErrorIs(t, err, ErrNotFound)
}
