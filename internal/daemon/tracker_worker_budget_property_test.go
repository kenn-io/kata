package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: reply waiting never extends the caller or the documented five
// minute budget after ticket expiry. The oracle uses the documented duration,
// independently of the production grace constant.
func TestTrackerWorkerReplyDeadlineIntersectionProperty(t *testing.T) {
	anchor := time.Now()
	hegel.Test(t, func(ht *hegel.T) {
		ticket := trackerWorkerTicketFixtureForResult()
		ticket.ExpiresAt = anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-600, 30))) * time.Second).Unix()
		parent, stop := context.WithDeadline(t.Context(), anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-60, 1500)))*time.Second))
		defer stop()
		expected := time.Unix(ticket.ExpiresAt, 0).Add(300 * time.Second)
		earlier, _ := parent.Deadline()
		if earlier.Before(expected) {
			expected = earlier
		}
		call, cancel := trackerWorkerReplyContext(parent, ticket)
		defer cancel()
		actual, ok := call.Deadline()
		require.True(ht, ok)
		require.Equal(ht, expected, actual)
	}, hegel.WithTestCases(100))
}

// Contract: an owner session cannot give one method more time than either the
// ticket reply budget or its parent invocation permits.
func TestTrackerWorkerOwnerDeadlineIntersectionProperty(t *testing.T) {
	anchor := time.Now()
	hegel.Test(t, func(ht *hegel.T) {
		ticket := trackerWorkerTicketFixtureForResult()
		ticket.ExpiresAt = anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-600, 30))) * time.Second).Unix()
		parent, stop := context.WithDeadline(t.Context(), anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-60, 1500)))*time.Second))
		defer stop()
		session := &trackerOwnerSession{ctx: t.Context(), hardDeadline: anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-60, 1500))) * time.Second)}
		expected := time.Unix(ticket.ExpiresAt, 0).Add(300 * time.Second)
		earlier, _ := parent.Deadline()
		if earlier.Before(expected) {
			expected = earlier
		}
		if session.hardDeadline.Before(expected) {
			expected = session.hardDeadline
		}
		method, cancel := trackerWorkerMethodContext(parent, session, ticket)
		defer cancel()
		actual, ok := method.Deadline()
		require.True(ht, ok)
		require.Equal(ht, expected, actual)
	}, hegel.WithTestCases(100))
}
