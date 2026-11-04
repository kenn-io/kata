// Package cronread checks that older daemons have not ignored run predicates.
package cronread

import (
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// VerifyFilters compares only known fields, allowing future daemon extensions.
// Legacy unfiltered and job-only reads do not require the new acknowledgement.
func VerifyFilters(raw []byte, want db.CronRunFilters) error {
	if len(want.Statuses) == 0 && want.IssueUID == "" && want.ExecutorLabel == "" && want.Teammate == "" && want.Since == nil {
		return nil
	}
	unsupported := errors.New("daemon does not support cron run filters; upgrade the daemon")
	var body struct {
		Filters *db.CronRunFilters `json:"filters"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return unsupported
	}
	got := body.Filters
	if got == nil || !slices.Equal(got.Statuses, want.Statuses) || got.JobUID != want.JobUID || got.IssueUID != want.IssueUID || got.ExecutorLabel != want.ExecutorLabel || got.Teammate != want.Teammate {
		return unsupported
	}
	if (got.Since == nil) != (want.Since == nil) {
		return unsupported
	}
	if got.Since != nil && !got.Since.Equal(*want.Since) {
		return unsupported
	}
	return nil
}
