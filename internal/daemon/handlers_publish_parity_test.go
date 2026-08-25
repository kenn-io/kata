package daemon_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
)

// drainBroadcastIDs collects the event ids of every event frame already queued
// on ch, stopping when the channel goes quiet for idle. Reset frames are
// recorded as 0 so a stray reset is visible in the comparison.
func drainBroadcastIDs(t *testing.T, ch <-chan daemon.StreamMsg, idle time.Duration) []int64 {
	t.Helper()
	var ids []int64
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return ids
			}
			if msg.Kind == daemon.StreamKindEvent {
				require.NotNil(t, msg.Event, "event frame must carry an event")
				ids = append(ids, msg.Event.ID)
				continue
			}
			ids = append(ids, 0)
		case <-time.After(idle):
			return ids
		}
	}
}

// TestMutationRoutesPublishToBothSinks drives representative mutation routes
// against a live broadcaster subscription and a recording hook sink, and
// asserts the same event ids arrive on both, in the same order. That pairing
// is the invariant EventPublisher owns; before it existed, dropping the hook
// enqueue at a single call site was invisible to every other test.
func TestMutationRoutesPublishToBothSinks(t *testing.T) {
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	h, pid := bootstrapProject(t, withHooksSink(sink), withBroadcaster(broadcaster))
	ts := h.ts.(*httptest.Server)
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: pid})
	defer sub.Unsub()

	resp, body := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "agent-1", "title": "parity", "body": "details"})
	require.Equalf(t, 200, resp.StatusCode, "create issue: %s", string(body))

	var created struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	ref := created.Issue.ShortID
	require.NotEmpty(t, ref)

	resp, body = postJSON(t, ts, issueURLRef(pid, ref, "comments"),
		map[string]any{"actor": "agent-1", "body": "a comment"})
	require.Equalf(t, 200, resp.StatusCode, "add comment: %s", string(body))

	resp, body = postJSON(t, ts, issueURLRef(pid, ref, "labels"),
		map[string]any{"actor": "agent-1", "label": "parity-label"})
	require.Equalf(t, 200, resp.StatusCode, "add label: %s", string(body))

	// The edit route emits several events in one call; their order is the
	// DB.EditIssueAtomic emission-order contract documented by editIssueHandler.
	resp, body = patchJSON(t, ts, issueURLRef(pid, ref, ""),
		map[string]any{"actor": "agent-1", "title": "parity edited", "set_priority": 1})
	require.Equalf(t, 200, resp.StatusCode, "edit issue: %s", string(body))

	broadcastIDs := drainBroadcastIDs(t, sub.Ch, 250*time.Millisecond)

	var hookIDs []int64
	for _, evt := range sink.snapshot() {
		hookIDs = append(hookIDs, evt.ID)
	}

	require.NotEmpty(t, broadcastIDs, "the driven routes must publish at least one event")
	assert.Equal(t, broadcastIDs, hookIDs,
		"every broadcast event must also reach the hook sink, in the same order")
}
