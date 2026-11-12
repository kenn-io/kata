package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"go.kenn.io/kata/internal/issuesync"
)

// NewSyncOutcomeObserver counts each binding or spoke's first failure until it recovers.
// State lives only in this daemon process, so a restart may count an outage twice.
func NewSyncOutcomeObserver(reporter TelemetryReporter) func(string, int64, error) {
	var mu sync.Mutex
	failing := map[string]bool{}
	return func(provider string, id int64, err error) {
		if reporter == nil || errors.Is(err, context.Canceled) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := fmt.Sprintf("%s:%d", provider, id)
		if err == nil {
			delete(failing, key)
			return
		}
		if failing[key] {
			return
		}
		failing[key] = reporter.Capture("sync_failed", map[string]any{"sync_source": provider, "reason": syncFailureReason(err)}) == nil
	}
}

func syncFailureReason(err error) string {
	status, limited, auth := 0, false, false
	if typed, ok := errors.AsType[*issuesync.StatusError](err); ok {
		status, limited, auth = typed.HTTPStatus, typed.RateLimited, typed.AuthFailure
	}
	if hub, ok := errors.AsType[interface {
		error
		HTTPStatus() int
	}](err); ok {
		status = hub.HTTPStatus()
	}
	switch {
	case status >= 500 && status <= 599:
		return "server"
	case limited || status == 429:
		return "rate_limit"
	case auth || status == 401 || status == 403:
		return "auth"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	if network, ok := errors.AsType[net.Error](err); ok {
		if network.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "other"
}
