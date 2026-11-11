package db

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// Six times the input should stay well below quadratic growth. Minimum samples
// reduce scheduler noise; the generous bound allows allocation and cache costs.
func TestFoldSnapshotLabelsDoNotScaleQuadratically(t *testing.T) {
	minimum := func(count int) time.Duration {
		events := make([]FoldEvent, count)
		for i := range events {
			events[i] = testEvent("issue.snapshot", int64(i+1), fmt.Sprintf(`{"uid":"issue-%d","title":"baseline","status":"open","labels":["alpha","beta"]}`, i))
			events[i].IssueUID = fmt.Sprintf("issue-%d", i)
		}
		run := func() { require.Len(t, FoldEvents(events).Labels, 2*count) }
		run()
		best := time.Duration(1<<63 - 1)
		for range 5 {
			started := time.Now()
			run()
			best = min(best, time.Since(started))
		}
		return best
	}
	small, large := minimum(2000), minimum(12000)
	t.Logf("snapshot fold minimum: 2000=%s 12000=%s ratio=%.2f", small, large, float64(large)/float64(small))
	require.Less(t, large, 14*small, "snapshot label retirement must not scale quadratically")
}
