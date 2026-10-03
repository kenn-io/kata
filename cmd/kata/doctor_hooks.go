package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"

	"go.kenn.io/kata/internal/diagnostics"
	kataapi "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (s *doctorState) hookChecks(ctx context.Context, apiClient *kataapi.Client) {
	var h generated.Hooks
	s.add("daemon.hooks", "hooks", func() diagnostics.Check {
		if apiClient == nil {
			return skippedDoctorCheck("Daemon connection could not be verified")
		}
		response, err := apiClient.DoctorWithResponse(ctx)
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		if err != nil || response == nil || status != http.StatusOK || response.JSON200 == nil || !response.JSON200.Hooks.Available {
			return diagnostics.Check{Status: "warn", Summary: "Live hook diagnostics are unsupported, unavailable or not visible to this principal", Details: doctorHTTPStatus(status), Fix: "Use operator credentials and a daemon with hook diagnostics support; custom hook sinks may not provide diagnostics."}
		}
		h = response.JSON200.Hooks
		check := diagnostics.Check{Status: "ok", Summary: "Active hook executables and working directories are available to the daemon"}
		if len(h.Hooks) == 0 {
			check.Status, check.Summary = "info", "Selected daemon has no active hooks"
		}
		for _, hook := range h.Hooks {
			if !hook.ExecutableAvailable {
				check.Status = "fail"
				check.Details = append(check.Details, fmt.Sprintf("hook %d command %q is unavailable to the daemon", hook.Index, hook.Command))
				if _, err := exec.LookPath(hook.Command); err == nil {
					check.Details = append(check.Details, "The invoking CLI can find this command; compare the daemon's PATH and filesystem with the invoking shell.")
				}
			}
			if !hook.WorkingDirectoryAvailable {
				check.Status = "fail"
				check.Details = append(check.Details, fmt.Sprintf("hook %d working directory is unavailable to the daemon", hook.Index))
			}
		}
		if check.Status == "fail" {
			check.Summary = "Active hooks have unavailable executables or working directories"
			check.Fix = "Correct hook commands, working directories or the daemon's PATH on the daemon host; explicitly reload hooks after changing configuration."
		}
		return check
	})
	s.add("daemon.hook_queue", "hooks", func() diagnostics.Check {
		if !h.Available {
			return skippedDoctorCheck("Live hook diagnostics are unavailable")
		}
		check := diagnostics.Check{Status: "ok", Summary: "Hook queue reports no dropped events or saturation", Details: []string{fmt.Sprintf("queued: %d/%d; in flight: %d; dropped since daemon start: %d", h.QueueLength, h.QueueCapacity, h.InFlight, h.Dropped)}}
		if h.Dropped > 0 || (h.QueueCapacity > 0 && h.QueueLength >= h.QueueCapacity) {
			check.Status, check.Summary = "warn", "Hook queue is saturated or has dropped events since daemon start"
			check.Fix = "Inspect hook execution time and daemon logs; reduce hook latency or tune hook queue capacity."
		}
		return check
	})
	s.add("daemon.hook_runs", "hooks", func() diagnostics.Check {
		if !h.Available {
			return skippedDoctorCheck("Live hook diagnostics are unavailable")
		}
		check := diagnostics.Check{Status: "ok", Summary: "Retained hook runs report no failures in the last seven days", Details: []string{fmt.Sprintf("retained runs: %d; failures: %d; history incomplete: %t", h.RecentRuns, h.RecentFailures, h.HistoryIncomplete)}}
		if h.RecentRuns == 0 {
			check.Status, check.Summary = "info", "No hook runs in the last seven days were found in retained history"
		}
		if h.RecentFailures > 0 || h.HistoryIncomplete {
			check.Status, check.Summary = "warn", "Retained hook history has recent failures or incomplete coverage"
			check.Fix = "Inspect retained hook run logs on the daemon host; historical indices do not identify the current hook configuration."
		}
		return check
	})
}
