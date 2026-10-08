package notification

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBroadcastRecipientsBoundsAndIdentity(t *testing.T) {
	candidates := []Identity{{Actor: "sender"}, {Actor: "sender", Teammate: "other"}, {Actor: "reader"}, {Actor: "reader", Teammate: "review"}, {Actor: "system"}, {Actor: "github-sync"}, {Actor: "notion-sync"}, {Actor: "plane-sync"}}
	got, e := BroadcastRecipients(candidates, Identity{Actor: "sender"}, false)
	require.NoError(t, e)
	require.Equal(t, []string{"reader"}, got)
	got, e = BroadcastRecipients(candidates, Identity{Actor: "sender"}, true)
	require.NoError(t, e)
	require.Equal(t, []string{"reader", "reader/review", "sender/other"}, got)
	candidates = nil
	for i := range 50 {
		candidates = append(candidates, Identity{Actor: fmt.Sprintf("worker-%02d", i)})
	}
	got, e = BroadcastRecipients(candidates, Identity{Actor: "sender"}, false)
	require.NoError(t, e)
	require.Len(t, got, 50)
	candidates = append(candidates, Identity{Actor: "one-too-many"})
	_, e = BroadcastRecipients(candidates, Identity{Actor: "sender"}, false)
	require.Error(t, e)
}

func TestBroadcastRateWindows(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name                    string
		records                 []BroadcastRecord
		sender, message, window string
		retry                   int
	}{
		{"sender", []BroadcastRecord{{At: now.Add(-time.Minute), Sender: "worker/review", Message: "first"}}, "worker/review", "second", "10m", 540},
		{"sender boundary", []BroadcastRecord{{At: now.Add(-10 * time.Minute), Sender: "worker", Message: "first"}}, "worker", "second", "", 0},
		{"duplicate", []BroadcastRecord{{At: now.Add(-20 * time.Minute), Sender: "worker", Message: "same"}}, "worker", "same", "1h", 2400},
		{"other teammate", []BroadcastRecord{{At: now.Add(-time.Minute), Sender: "worker/review", Message: "same"}}, "worker/builder", "same", "", 0},
		{"issue", []BroadcastRecord{{At: now.Add(-59 * time.Minute), Sender: "a"}, {At: now.Add(-30 * time.Minute), Sender: "b"}, {At: now.Add(-time.Minute), Sender: "c"}}, "worker", "new", "1h", 60},
		{"hour boundary", []BroadcastRecord{{At: now.Add(-time.Hour), Sender: "worker", Message: "same"}}, "worker", "same", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := CheckBroadcastRate(c.records, c.sender, c.message, now)
			if c.window == "" {
				require.Nil(t, e)
			} else {
				require.NotNil(t, e)
				require.Equal(t, c.window, e.Window)
				require.Equal(t, c.retry, e.RetryAfterSeconds)
				require.NotEmpty(t, e.LastBroadcastBy)
				require.False(t, e.LastBroadcastAt.IsZero())
			}
		})
	}
}

func TestBroadcastRateErrorRedactsHiddenLatestRecordDetails(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	records := []BroadcastRecord{
		{At: now.Add(-30 * time.Minute), Sender: "visible", Message: "visible history"},
		{At: now.Add(-10 * time.Minute), Sender: "hidden/older", Message: "older hidden", Hidden: true},
		{At: now.Add(-time.Minute), Sender: "hidden/reviewer", Message: "private message", Re: "out-of-scope", Hidden: true},
	}
	err := CheckBroadcastRate(records, "caller", "new message", now)
	require.NotNil(t, err)
	require.Equal(t, "1h", err.Window)
	require.NotEmpty(t, err.RetryAfterSeconds)
	require.True(t, err.LastBroadcastAt.IsZero())
	require.Empty(t, err.LastBroadcastBy)
	require.Empty(t, err.LastMessagePrefix)
}

func FuzzBroadcastRecipients(f *testing.F) {
	f.Add(uint8(51), true)
	f.Fuzz(func(t *testing.T, n uint8, teammates bool) {
		candidates := []Identity{{Actor: "sender"}, {Actor: "system"}, {Actor: "github-sync"}}
		for i := range int(n) {
			candidates = append(candidates, Identity{Actor: fmt.Sprintf("worker-%03d", i)}, Identity{Actor: fmt.Sprintf("worker-%03d", i)})
		}
		got, e := BroadcastRecipients(candidates, Identity{Actor: "sender"}, teammates)
		if n > 50 {
			require.Error(t, e)
			return
		}
		require.NoError(t, e)
		require.Len(t, got, int(n))
		seen := map[string]bool{}
		for _, r := range got {
			require.NotEqual(t, "sender", r)
			require.NotEqual(t, "system", r)
			require.False(t, seen[r])
			seen[r] = true
		}
	})
}
