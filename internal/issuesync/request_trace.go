package issuesync

import (
	"context"
	"net/http/httptrace"
	"sync/atomic"
)

// TrackRequestWrite returns a context that records whether a request's headers
// reached the connection. A DNS, dial, or TLS failure leaves the report false,
// so a status write that failed that early was never sent and is not ambiguous.
func TrackRequestWrite(ctx context.Context) (context.Context, func() bool) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }}
	return httptrace.WithClientTrace(ctx, trace), wrote.Load
}
