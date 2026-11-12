package client

import (
	"context"
	"net/http"

	"go.kenn.io/kata/internal/uid"
)

const (
	eventFeaturesHeader          = "X-Kata-Event-Features"
	cronWorkflowArgvEventFeature = "cron_workflow_argv_v1"
)

// NewCronUID generates a normalized ULID for a cron definition or run
// observation. Generate it once, persist it with the intended request, and
// reuse both after a timeout. Client calls never replace request identities or
// treat a conflict as a successful create. A new definition or run needs a new
// UID. This function makes no network request.
func NewCronUID() (string, error) { return uid.New() }

// CronWorkflowArgvSupport declares that a client preserves literal argv steps.
// It does not grant permission to execute a workflow.
func CronWorkflowArgvSupport(_ context.Context, req *http.Request) error {
	req.Header.Set(eventFeaturesHeader, cronWorkflowArgvEventFeature)
	return nil
}
